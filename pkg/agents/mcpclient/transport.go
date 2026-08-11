package mcpclient

import (
	"context"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// sdkClient is the package-level MCP client factory. The official SDK
// separates the reusable client implementation from a live connection
// (a *mcp.ClientSession), so a single shared client is sufficient — each
// Connect call yields its own session.
var sdkClient = mcp.NewClient(&mcp.Implementation{
	Name:    "hastekit-sdk-go",
	Version: "0.1.0",
}, &mcp.ClientOptions{
	ProgressNotificationHandler: handleProgressNotification,
	// The multi-round-trip middleware answers a server's input request from a
	// callback and retries the call immediately. An agent cannot: the answer
	// comes from a person, on a later request, after the run has paused. So
	// the tool drives the exchange itself — see elicitation.go.
	MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
	// Capabilities are declared rather than inferred. Inference keys off an
	// ElicitationHandler, which is only that middleware's callback and so is
	// not registered, and it advertises form elicitation only — a server
	// refuses url mode unless it is declared.
	// Roots is carried over unchanged from the SDK default so declaring
	// these does not quietly drop it.
	Capabilities: &mcp.ClientCapabilities{
		Elicitation: &mcp.ElicitationCapabilities{
			Form: &mcp.FormElicitationCapabilities{},
			URL:  &mcp.URLElicitationCapabilities{},
		},
		RootsV2: &mcp.RootCapabilities{ListChanged: true},
	},
})

// headerRoundTripper injects a fixed set of headers onto every outgoing
// request. The official SDK transports don't expose a headers option, so
// we layer them on via a custom http.Client transport.
type headerRoundTripper struct {
	headers map[string]string
	base    http.RoundTripper
}

func (h *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if len(h.headers) > 0 {
		req = req.Clone(req.Context())
		for k, v := range h.headers {
			req.Header.Set(k, v)
		}
	}
	base := h.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// httpClientWithHeaders returns an *http.Client that adds the given
// headers to every request, or nil when there are no headers (so the
// transport falls back to http.DefaultClient).
func httpClientWithHeaders(headers map[string]string) *http.Client {
	if len(headers) == 0 {
		return nil
	}
	return &http.Client{Transport: &headerRoundTripper{headers: headers}}
}

// newClientTransport builds the right SDK transport for the configured
// transport type, with custom headers layered on via the HTTP client.
//
// disableStandaloneSSE skips the post-init GET that opens a server→client
// SSE stream on the streamable-http transport. We only do request/response
// tool calls, so the stream is unused; some servers never answer that GET,
// leaving the client hung waiting on a stream that never opens. Callers
// opt those servers out via WithDisableStandaloneSSE.
func newClientTransport(endpoint, transportType string, headers map[string]string, disableStandaloneSSE bool) mcp.Transport {
	hc := httpClientWithHeaders(headers)
	switch transportType {
	case "streamable-http":
		return &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: hc, DisableStandaloneSSE: disableStandaloneSSE}
	default:
		return &mcp.SSEClientTransport{Endpoint: endpoint, HTTPClient: hc}
	}
}

// connect opens a live MCP session over the given transport. Connect
// performs the initialize handshake internally (no separate Start/Initialize).
func connect(ctx context.Context, endpoint, transportType string, headers map[string]string, disableStandaloneSSE bool) (*mcp.ClientSession, error) {
	return sdkClient.Connect(ctx, newClientTransport(endpoint, transportType, headers, disableStandaloneSSE), nil)
}
