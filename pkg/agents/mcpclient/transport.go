package mcpclient

import (
	"context"
	"net/http"
	"os"
	"os/exec"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// sdkClient is the package-level MCP client factory. The official SDK
// separates the reusable client implementation from a live connection
// (a *mcp.ClientSession), so a single shared client is sufficient — each
// Connect call yields its own session.
var sdkClient = mcp.NewClient(&mcp.Implementation{
	Name:    "agent-sdk-go",
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

// Transport names accepted by WithTransport.
const (
	TransportSSE            = "sse"
	TransportStreamableHTTP = "streamable-http"

	// TransportStdio runs the server as a child process and speaks to it over
	// its stdin and stdout, which is how servers that ship as a command rather
	// than a URL are reached.
	TransportStdio = "stdio"
)

// newClientTransport builds the right SDK transport for the configured
// transport type: a child process for stdio, otherwise an HTTP transport with
// custom headers layered on via the HTTP client.
//
// DisableStandaloneSSE skips the post-init GET that opens a server→client
// SSE stream on the streamable-http transport. We only do request/response
// tool calls, so the stream is unused; some servers never answer that GET,
// leaving the client hung waiting on a stream that never opens. Callers
// opt those servers out via WithDisableStandaloneSSE.
//
// ctx bounds a stdio server's process: it is killed when ctx is done. The pool
// connects on context.Background() for exactly that reason — see Checkout.
func newClientTransport(ctx context.Context, conn serverConn) mcp.Transport {
	if conn.isStdio() {
		cmd := exec.CommandContext(ctx, conn.Command[0], conn.Command[1:]...)
		cmd.Env = conn.childEnv()
		// A server writing diagnostics to stderr should not be silently
		// swallowed; its stdout is the protocol and stays untouched.
		cmd.Stderr = os.Stderr
		return &mcp.CommandTransport{Command: cmd}
	}

	hc := httpClientWithHeaders(conn.Headers)
	switch conn.Transport {
	case TransportStreamableHTTP:
		return &mcp.StreamableClientTransport{Endpoint: conn.Endpoint, HTTPClient: hc, DisableStandaloneSSE: conn.DisableStandaloneSSE}
	default:
		return &mcp.SSEClientTransport{Endpoint: conn.Endpoint, HTTPClient: hc}
	}
}

// connect opens a live MCP session over the given transport. Connect
// performs the initialize handshake internally (no separate Start/Initialize).
func connect(ctx context.Context, conn serverConn) (*mcp.ClientSession, error) {
	if err := conn.validate(); err != nil {
		return nil, err
	}
	return sdkClient.Connect(ctx, newClientTransport(ctx, conn), nil)
}
