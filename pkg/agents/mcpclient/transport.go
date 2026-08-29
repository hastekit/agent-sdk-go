package mcpclient

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"sync"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
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

// authStatus is the authorization status an http server answered with, kept
// from the one place it still exists.
//
// The MCP authorization spec is specific about these: a server that wants
// credentials answers 401, and one that has them but finds them insufficient
// answers 403. Neither code survives the SDK's transports, which report a
// non-2xx as its status text alone — by the time an error reaches connect,
// a 401 is the word "Unauthorized" inside a message. So the response is read
// here, while it is still a response.
type authStatus struct {
	mu   sync.Mutex
	code int
}

func (a *authStatus) record(code int) {
	if code != http.StatusUnauthorized && code != http.StatusForbidden {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.code = code
}

// refused reports whether the server turned our credentials away. Nil-safe:
// stdio has no status to record and no credentials to be refused.
func (a *authStatus) refused() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.code != 0
}

// mcpRoundTripper does the two things the SDK's http transports leave to the
// http client: it sets our headers on every request, and it keeps the
// authorization status of what came back.
type mcpRoundTripper struct {
	headers map[string]string
	auth    *authStatus
	base    http.RoundTripper
}

func (h *mcpRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
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
	resp, err := base.RoundTrip(req)
	if resp != nil {
		h.auth.record(resp.StatusCode)
	}
	return resp, err
}

// newHTTPClient returns the client an http transport speaks through, and the
// authorization status it will record into.
func newHTTPClient(headers map[string]string) (*http.Client, *authStatus) {
	auth := &authStatus{}
	return &http.Client{Transport: &mcpRoundTripper{headers: headers, auth: auth}}, auth
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
// It returns the authStatus the transport records into alongside it, which is
// nil for stdio — a child process has no HTTP status to answer with.
func newClientTransport(ctx context.Context, conn serverConn) (mcp.Transport, *authStatus) {
	if conn.isStdio() {
		cmd := exec.CommandContext(ctx, conn.Command[0], conn.Command[1:]...)
		cmd.Env = conn.childEnv()
		// A server writing diagnostics to stderr should not be silently
		// swallowed; its stdout is the protocol and stays untouched.
		cmd.Stderr = os.Stderr
		return &mcp.CommandTransport{Command: cmd}, nil
	}

	hc, auth := newHTTPClient(conn.Headers)
	switch conn.Transport {
	case TransportStreamableHTTP:
		return &mcp.StreamableClientTransport{Endpoint: conn.Endpoint, HTTPClient: hc, DisableStandaloneSSE: conn.DisableStandaloneSSE}, auth
	default:
		return &mcp.SSEClientTransport{Endpoint: conn.Endpoint, HTTPClient: hc}, auth
	}
}

// connect opens a live MCP session over the given transport. Connect
// performs the initialize handshake internally (no separate Start/Initialize).
func connect(ctx context.Context, conn serverConn) (*mcp.ClientSession, error) {
	if err := conn.validate(); err != nil {
		return nil, err
	}

	transport, auth := newClientTransport(ctx, conn)

	session, err := sdkClient.Connect(ctx, transport, nil)
	if err != nil {
		// A server that refused our credentials is something the user can put
		// right, and the layer above says so in the agent's prompt. Everything
		// else is just a server that is not there.
		if auth.refused() {
			return nil, agents.NewToolsetError(agents.ToolsetErrorAuth, err)
		}
		return nil, err
	}

	return session, nil
}
