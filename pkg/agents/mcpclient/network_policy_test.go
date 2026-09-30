package mcpclient

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

// replaceUserTransport installs a test socket boundary without changing the public-address policy.
// These fixtures run sequentially; each constructed client retains its own transport snapshot.
func replaceUserTransport(t *testing.T, dialer publicDialer) *http.Transport {
	t.Helper()
	previous := userHTTPTransport
	transport := newPublicTransport(dialer.DialContext)
	userHTTPTransport = transport
	t.Cleanup(func() {
		userHTTPTransport = previous
		transport.CloseIdleConnections()
	})
	return transport
}

// userCatalogServer presents a public address while routing vetted test sockets to a local fixture.
func userCatalogServer(t *testing.T, label string) string {
	t.Helper()
	endpoint, err := url.Parse(catalogServer(t, label))
	require.NoError(t, err)
	replaceUserTransport(t, publicDialer{
		lookup: func(context.Context, string, string) ([]netip.Addr, error) {
			return nil, errors.New("unexpected DNS lookup in catalog fixture")
		},
		dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil || host != "93.184.216.34" {
				return nil, errors.New("unexpected destination in catalog fixture")
			}
			return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
		},
	})
	endpoint.Host = net.JoinHostPort("93.184.216.34", endpoint.Port())
	return endpoint.String()
}

func TestPublicURLPolicy(t *testing.T) {
	// Reject local and special-use addresses, including mapped and translated IPv4 forms.
	for _, host := range []string{
		"localhost", "LOCALHOST.", "service.localhost", "service.local", "service",
		"127.0.0.1", "127.1.2.3", "0.0.0.0", "10.1.2.3", "172.16.0.1", "192.168.1.1",
		"100.64.0.1", "169.254.169.254", "168.63.129.16", "198.18.0.1", "192.0.2.1",
		"198.51.100.1", "203.0.113.1", "224.0.0.1", "255.255.255.255",
		"[::]", "[::1]", "[::ffff:127.0.0.1]", "[fc00::1]", "[fe80::1]", "[ff02::1]",
		"[fe80::1%25en0]", "[64:ff9b::a00:1]", "[2002:7f00:1::]", "[2001:db8::1]",
	} {
		t.Run(host, func(t *testing.T) {
			require.Error(t, validatePublicURL("https://"+host+"/mcp"))
		})
	}

	// Public endpoints remain valid without requiring network availability during registration.
	for _, endpoint := range []string{"https://mcp.example/mcp", "http://93.184.216.34:8080/mcp", "https://[2606:4700:4700::1111]/mcp"} {
		require.NoError(t, validatePublicURL(endpoint))
	}
	for _, endpoint := range []string{"file:///etc/passwd", "ftp://public.example/mcp", "https://user:pass@public.example", "https://public.example/#fragment"} {
		require.Error(t, validatePublicURL(endpoint))
	}
}

func TestPublicDialerValidatesAllAnswersAndPinsAddress(t *testing.T) {
	// A mixed DNS answer must fail before any socket is opened, regardless of address order.
	for _, addresses := range [][]netip.Addr{
		{netip.MustParseAddr("127.0.0.1")},
		{netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("10.0.0.1")},
		{netip.MustParseAddr("::ffff:169.254.169.254"), netip.MustParseAddr("93.184.216.34")},
		nil,
	} {
		dialer := publicDialer{
			lookup: func(context.Context, string, string) ([]netip.Addr, error) { return addresses, nil },
			dial: func(context.Context, string, string) (net.Conn, error) {
				t.Fatal("private DNS answer reached the socket dialer")
				return nil, nil
			},
		}
		_, err := dialer.DialContext(t.Context(), "tcp", "mcp.example:443")
		require.ErrorIs(t, err, errPrivateEndpoint)
	}

	// A changing resolver cannot replace the vetted IP during the subsequent socket connection.
	lookups := 0
	client, peer := net.Pipe()
	defer peer.Close()
	dialer := publicDialer{
		lookup: func(_ context.Context, network, host string) ([]netip.Addr, error) {
			lookups++
			require.Equal(t, "ip", network)
			require.Equal(t, "mcp.example.", host)
			if lookups > 1 {
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			}
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		},
		dial: func(_ context.Context, network, address string) (net.Conn, error) {
			require.Equal(t, "tcp", network)
			require.Equal(t, "93.184.216.34:443", address)
			return client, nil
		},
	}
	conn, err := dialer.DialContext(t.Context(), "tcp", "mcp.example:443")
	require.NoError(t, err)
	conn.Close()
	require.Equal(t, 1, lookups)
	_, err = dialer.DialContext(t.Context(), "tcp", "mcp.example:443")
	require.ErrorIs(t, err, errPrivateEndpoint)
}

func TestUserMCPTransportBlocksPrivateDNSAndSSEOrigins(t *testing.T) {
	// Even a public-looking hostname is blocked when DNS resolves to the cluster network.
	var sockets atomic.Int32
	transport := replaceUserTransport(t, publicDialer{
		lookup: func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("10.0.0.3")}, nil
		},
		dial: func(context.Context, string, string) (net.Conn, error) {
			sockets.Add(1)
			return nil, errors.New("unexpected dial")
		},
	})
	require.Nil(t, transport.Proxy, "environment proxies must not bypass address validation")
	client, _ := newHTTPClient(serverConn{Namespace: "alice", Endpoint: "https://public.example/mcp"})
	_, err := client.Get("https://public.example/mcp")
	require.ErrorIs(t, err, errPrivateEndpoint)
	require.Zero(t, sockets.Load())

	// SSE post URLs and redirects cannot move authenticated requests to another origin.
	_, err = client.Get("https://another.example/messages")
	require.ErrorContains(t, err, "configured origin")
	require.Zero(t, sockets.Load())
}

func TestUserRedirectsCannotReachPrivateEndpoints(t *testing.T) {
	// Exercise real HTTP redirects through the policy while socket routing stays inside the fixture.
	var privateRequests atomic.Int32
	private := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		privateRequests.Add(1)
		_, _ = io.WriteString(w, "internal secret")
	}))
	defer private.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, private.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	replaceUserTransport(t, publicDialer{
		lookup: func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		},
		dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(redirect.URL, "http://"))
		},
	})
	client, _ := newHTTPClient(serverConn{Namespace: "alice", Endpoint: "http://public.example/mcp"})
	_, err := client.Get("http://public.example/mcp")
	require.ErrorContains(t, err, "another origin")
	require.Zero(t, privateRequests.Load())

	// Same-origin redirects remain usable; protocol downgrades and public-origin changes fail.
	previous := httptest.NewRequest("GET", "https://public.example/mcp", nil)
	require.NoError(t, sameOriginRedirect(httptest.NewRequest("GET", "https://public.example/next", nil), []*http.Request{previous}))
	require.Error(t, sameOriginRedirect(httptest.NewRequest("GET", "http://public.example/next", nil), []*http.Request{previous}))
	require.Error(t, sameOriginRedirect(httptest.NewRequest("GET", "https://other.example/next", nil), []*http.Request{previous}))
}

func TestUserOAuthExchangesAndRefreshesUsePublicTransport(t *testing.T) {
	// User token URLs cannot inherit a trusted client's transport or proxy behavior.
	var sockets atomic.Int32
	replaceUserTransport(t, publicDialer{
		lookup: func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
		},
		dial: func(context.Context, string, string) (net.Conn, error) {
			sockets.Add(1)
			return nil, errors.New("unexpected token dial")
		},
	})
	provider := authorizationProvider(t)
	connector := Connector{Name: "mail", Namespace: "alice", Endpoint: "https://mcp.example/mcp", Authorization: oauthTestConfig("https://tokens.example/token")}
	connector.Authorization.RedirectURL = "https://app.example/callback"
	config, err := resolveOAuthConfig(connector)
	require.NoError(t, err)
	ctx, cancel := provider.tokenContext(t.Context(), config)
	defer cancel()
	require.NotSame(t, provider.client, ctx.Value(oauth2.HTTPClient))
	_, err = config.OAuth.Exchange(ctx, "code")
	require.ErrorIs(t, err, errPrivateEndpoint)

	// The persisted source uses the same protected path when renewing an expired token.
	require.NoError(t, provider.store.Save(t.Context(), "alice", OAuthCredentialKey(connector), &oauth2.Token{AccessToken: "expired", RefreshToken: "refresh-secret", Expiry: time.Now().Add(-time.Hour)}))
	credential, err := provider.Resolve(t.Context(), "alice", connector, nil)
	require.NoError(t, err)
	_, err = credential.TokenSource.Token()
	require.ErrorContains(t, err, "refresh failed")
	require.Zero(t, sockets.Load())

	// Global definitions retain trusted custom clients and local development support.
	config.Namespace = ""
	globalContext, globalCancel := provider.tokenContext(t.Context(), config)
	defer globalCancel()
	require.Same(t, provider.client, globalContext.Value(oauth2.HTTPClient))
}

func TestUserOAuthCallbackBlocksPrivateDNS(t *testing.T) {
	// Complete browser state validation before exercising the callback's actual exchange path.
	var sockets atomic.Int32
	replaceUserTransport(t, publicDialer{
		lookup: func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
		dial: func(context.Context, string, string) (net.Conn, error) {
			sockets.Add(1)
			return nil, errors.New("unexpected callback dial")
		},
	})
	provider := authorizationProvider(t)
	store := NewMemoryStore()
	config := ServerConfig{Name: "mail", Endpoint: "https://mcp.example", Authorization: oauthTestConfig("https://tokens.example/token")}
	config.Authorization.RedirectURL = "https://app.example/mail/callback"
	require.NoError(t, store.Put(t.Context(), "alice", config))
	handler := NewHandler(store, WithOAuth(provider), WithNamespaceResolver(func(*http.Request) (string, error) { return "alice", nil }))
	connect := httptest.NewRecorder()
	handler.ServeHTTP(connect, httptest.NewRequest("GET", "/mail/connect", nil))
	require.Equal(t, http.StatusFound, connect.Code)
	location, err := url.Parse(connect.Header().Get("Location"))
	require.NoError(t, err)
	cookies := connect.Result().Cookies()
	require.Len(t, cookies, 1)

	// DNS resolved at exchange time cannot cause an internal request or create a credential record.
	request := httptest.NewRequest("GET", "/mail/callback?code=code&state="+url.QueryEscape(location.Query().Get("state")), nil)
	request.AddCookie(cookies[0])
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	require.Equal(t, http.StatusBadGateway, response.Code)
	require.Zero(t, sockets.Load())
	config.Namespace = "alice"
	_, err = provider.store.Load(t.Context(), "alice", OAuthCredentialKey(Connector{Name: config.Name, Namespace: config.Namespace, Authorization: config.Authorization}))
	require.ErrorIs(t, err, ErrCredentialNotFound)
}
