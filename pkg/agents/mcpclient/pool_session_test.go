package mcpclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

// What the pool holds for a streamable-http server is never a TCP connection —
// http.Transport already keeps those alive on its own. What it holds depends on
// what the server speaks.
//
// Against a server on 2025-11-25 or earlier it is an MCP session: an
// Mcp-Session-Id the server allocated, paid for with an initialize handshake
// and released with a DELETE. Against a 2026-07-28 server there is no session
// at all (SEP-2567, SEP-2575) and what is saved is one server/discover round
// trip per call. Both are worth having; only the first is worth much.
//
// The go-sdk's handler is stateful unless asked otherwise, so both shapes are
// exercised here.
type sessionCounter struct {
	mu       sync.Mutex
	requests int
	sessions map[string]bool
}

func countingMCPServer(t *testing.T) (string, *sessionCounter) {
	return countingMCPServerMode(t, false)
}

func countingMCPServerMode(t *testing.T, stateless bool) (string, *sessionCounter) {
	t.Helper()
	count := &sessionCounter{sessions: map[string]bool{}}

	server := mcp.NewServer(&mcp.Implementation{Name: "counting", Version: "0.1.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
		})

	inner := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: stateless})
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.mu.Lock()
		count.requests++
		if sid := r.Header.Get("Mcp-Session-Id"); sid != "" {
			count.sessions[sid] = true
		}
		count.mu.Unlock()
		// Passed straight through: buffering the response would stall a
		// handler that streams.
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { httpSrv.CloseClientConnections(); httpSrv.Close() })

	return httpSrv.URL, count
}

func callEcho(t *testing.T, conn serverConn, times int) {
	t.Helper()
	tool := NewLazyMcpTool(&mcp.Tool{Name: "echo"}, conn, nil, false, false, "")
	for i := 0; i < times; i++ {
		_, err := tool.Execute(context.Background(), &agents.ToolCall{
			FunctionCallMessage: &responses.FunctionCallMessage{
				ID: "fc_c", CallID: "c", Name: "echo", Arguments: "{}",
			},
		})
		require.NoError(t, err)
	}
}

func (c *sessionCounter) totals() (requests, sessions int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requests, len(c.sessions)
}

func TestPoolingReusesOneStreamableSession(t *testing.T) {
	url, count := countingMCPServer(t)
	conn := serverConn{Transport: TransportStreamableHTTP, Endpoint: url, DisableStandaloneSSE: true}
	t.Cleanup(func() { globalPool.Remove(conn) })

	callEcho(t, conn, 3)

	_, sessions := count.totals()
	assert.Equal(t, 1, sessions, "three calls on one pooled connection are one session on the server")
}

// The unpooled path is correct but not free, which is the cost a provider pays
// for not naming its principal — see Credential.Principal.
func TestUnpooledCredentialsOpenASessionPerCall(t *testing.T) {
	url, count := countingMCPServer(t)
	conn := serverConn{
		Transport: TransportStreamableHTTP, Endpoint: url, DisableStandaloneSSE: true,
		TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "t", TokenType: "Bearer"}),
	}
	require.False(t, conn.poolable())

	callEcho(t, conn, 3)

	requests, sessions := count.totals()
	assert.Equal(t, 3, sessions, "each call authenticates a session of its own")
	assert.Greater(t, requests, 12, "and pays an initialize handshake and a delete for each")
}

// Pooling is not an SSE-only concern: a streamable-http session costs a
// handshake to open and a DELETE to close, whichever transport carries it.
func TestPoolingSavesHandshakesOnStreamableHTTP(t *testing.T) {
	pooledURL, pooled := countingMCPServer(t)
	pooledConn := serverConn{Transport: TransportStreamableHTTP, Endpoint: pooledURL, DisableStandaloneSSE: true}
	t.Cleanup(func() { globalPool.Remove(pooledConn) })
	callEcho(t, pooledConn, 3)

	freshURL, fresh := countingMCPServer(t)
	freshConn := serverConn{
		Transport: TransportStreamableHTTP, Endpoint: freshURL, DisableStandaloneSSE: true,
		TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "t", TokenType: "Bearer"}),
	}
	callEcho(t, freshConn, 3)

	pooledRequests, _ := pooled.totals()
	freshRequests, _ := fresh.totals()
	assert.Less(t, pooledRequests, freshRequests,
		"reusing the session must cost fewer requests than rebuilding it")
}

// On a 2026-07-28 server there is no session to reuse — but the client still
// opens one server/discover per connection, so pooling is the difference
// between paying that once and paying it per call. Much less than it saves
// against a stateful server, and still not nothing.
func TestPoolingSavesDiscoveryOnAStatelessServer(t *testing.T) {
	pooledURL, pooled := countingMCPServerMode(t, true)
	pooledConn := serverConn{Transport: TransportStreamableHTTP, Endpoint: pooledURL, DisableStandaloneSSE: true}
	t.Cleanup(func() { globalPool.Remove(pooledConn) })
	callEcho(t, pooledConn, 3)

	freshURL, fresh := countingMCPServerMode(t, true)
	freshConn := serverConn{
		Transport: TransportStreamableHTTP, Endpoint: freshURL, DisableStandaloneSSE: true,
		TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "t", TokenType: "Bearer"}),
	}
	callEcho(t, freshConn, 3)

	pooledRequests, pooledSessions := pooled.totals()
	freshRequests, freshSessions := fresh.totals()

	assert.Zero(t, pooledSessions, "a stateless server issues no session id at all")
	assert.Zero(t, freshSessions)

	// One discover plus three calls, against three discovers plus three calls.
	assert.Equal(t, 4, pooledRequests)
	assert.Equal(t, 6, freshRequests)
}
