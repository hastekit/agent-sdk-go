package mcpclient

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

var errPrivateEndpoint = errors.New("user MCP endpoints must use public network addresses")

// deniedNetworks includes private, special-use, and IPv4 translation ranges.
// IPv6 is additionally restricted to the global unicast allocation below.
var deniedNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("168.63.129.16/32"), // Azure platform services.
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
}

// publicAddress fails closed for addresses that do not identify public destinations.
func publicAddress(address netip.Addr) bool {
	if !address.IsValid() || address.Zone() != "" {
		return false
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() {
		return false
	}
	if address.Is6() && !netip.MustParsePrefix("2000::/3").Contains(address) {
		return false
	}

	// Exclude special-use ranges even when Go classifies them as global unicast.
	for _, prefix := range deniedNetworks {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

// validatePublicURL checks syntax and literal destinations without doing DNS at registration.
// Hostnames are resolved and checked again at the actual connection boundary.
func validatePublicURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return errors.New("user MCP endpoints require an HTTP or HTTPS URL without userinfo or fragments")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if address, err := netip.ParseAddr(host); err == nil {
		if !publicAddress(address) {
			return errPrivateEndpoint
		}
		return nil
	}

	// Local names and scoped IPv6 addresses are never meaningful public endpoints.
	if !strings.Contains(host, ".") || strings.ContainsAny(host, ":%") || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return errPrivateEndpoint
	}
	return nil
}

// publicDialer resolves once, validates every answer, and dials the vetted IP directly.
// Keeping resolution separate from the socket dial prevents DNS rebinding between checks.
type publicDialer struct {
	lookup func(context.Context, string, string) ([]netip.Addr, error)
	dial   func(context.Context, string, string) (net.Conn, error)
}

func (d publicDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errPrivateEndpoint
	}
	ips := []netip.Addr{}
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = append(ips, ip)
	} else {
		// An absolute DNS name avoids resolver search paths such as cluster-local suffixes.
		ips, err = d.lookup(ctx, "ip", strings.TrimSuffix(host, ".")+".")
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errors.New("unable to resolve user MCP endpoint")
		}
	}

	// Reject mixed public/private responses before attempting any connection.
	if len(ips) == 0 {
		return nil, errPrivateEndpoint
	}
	for _, ip := range ips {
		if !publicAddress(ip) {
			return nil, errPrivateEndpoint
		}
	}

	// Retry only the already-vetted addresses; never give a hostname to the socket dialer.
	for _, ip := range ips {
		conn, dialErr := d.dial(ctx, network, net.JoinHostPort(ip.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, errors.New("unable to connect to user MCP endpoint")
}

// newPublicTransport uses direct connections so environment proxies cannot bypass IP checks.
// TLS still verifies the original URL hostname, while DialContext pins the vetted address.
func newPublicTransport(dial func(context.Context, string, string) (net.Conn, error)) *http.Transport {
	return &http.Transport{
		DialContext: dial, ForceAttemptHTTP2: true, MaxIdleConns: 100,
		IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// Share safe connection pools without inheriting mutable application default transports.
var userHTTPTransport = newPublicTransport(publicDialer{
	lookup: net.DefaultResolver.LookupNetIP,
	dial:   (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
}.DialContext)

// publicRoundTripper checks every request, including redirects and SSE-discovered endpoints.
type publicRoundTripper struct {
	base *http.Transport
}

func (p publicRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := validatePublicURL(req.URL.String()); err != nil {
		return nil, err
	}
	return p.base.RoundTrip(req)
}

// sameOriginRedirect prevents credentials from following an endpoint to another origin.
func sameOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if req.URL.Scheme != via[0].URL.Scheme || !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
		return errors.New("user MCP endpoints cannot redirect to another origin")
	}
	return validatePublicURL(req.URL.String())
}
