package mcpclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// lockedBuffer collects logs written from the transports' goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// records are the JSON log records written so far.
func (b *lockedBuffer) records(t *testing.T) (records []map[string]any, raw string) {
	t.Helper()
	b.mu.Lock()
	raw = b.buf.String()
	b.mu.Unlock()
	scanner := bufio.NewScanner(strings.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for scanner.Scan() {
		var record map[string]any
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &record))
		records = append(records, record)
	}
	return records, raw
}

// captureLogs sends the default logger's records to the returned buffer for
// the rest of the test.
func captureLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	logs := &lockedBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logs
}

// withMessage are the records logged with msg.
func withMessage(records []map[string]any, msg string) []map[string]any {
	var out []map[string]any
	for _, record := range records {
		if record["msg"] == msg {
			out = append(out, record)
		}
	}
	return out
}

// A listing logs each connection, each request it makes and each server it
// could or could not use; a tool call logs its server, tool, namespace and the
// headers it went with. Header values are logged as sent, an empty one too,
// and credentials never.
func TestClientLogsRequestsConnectionsAndToolCalls(t *testing.T) {
	logs := captureLogs(t)
	up := providerTestResource(t, "lookup", "", nil)
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()

	headers := map[string]string{
		"x-account-id":    "acc-1",
		"x-user-email-id": "",
		"x-api-key":       "secret-key",
		"apikey":          "secret-apikey",
		"Authorization":   "Bearer secret-bearer",
	}
	client := NewClient(NewInlineStore(
		ServerConfig{Name: "adp", Endpoint: up.URL, Transport: TransportStreamableHTTP, Headers: headers},
		ServerConfig{Name: "down", Endpoint: down.URL, Transport: TransportStreamableHTTP},
	))
	statuses, tools, err := client.ListTools(t.Context(), "acc-alice", nil)
	require.NoError(t, err)
	require.Len(t, statuses, 2)
	require.Len(t, tools, 1)
	_, err = tools[0].Execute(t.Context(), &agents.ToolCall{
		Namespace:           "acc-alice",
		FunctionCallMessage: &responses.FunctionCallMessage{Name: "lookup", Arguments: "{}", CallID: "call-1"},
	})
	require.NoError(t, err)

	records, raw := logs.records(t)
	require.NotContains(t, raw, "secret", "a credential reached the logs")
	wantHeaders := func(t *testing.T, record map[string]any) {
		t.Helper()
		logged, ok := record["headers"].(map[string]any)
		require.True(t, ok, "no headers in %v", record)
		require.Equal(t, "acc-1", logged["x-account-id"])
		require.Contains(t, logged, "x-user-email-id", "an empty header is still logged")
		require.Equal(t, "", logged["x-user-email-id"])
		for _, secret := range []string{"x-api-key", "apikey", "authorization"} {
			require.Equal(t, redacted, logged[secret], secret)
		}
	}

	connected := withMessage(records, "MCP connected")
	require.NotEmpty(t, connected)
	require.Equal(t, "adp", connected[0]["server"])
	wantHeaders(t, connected[0])

	var calls int
	for _, request := range withMessage(records, "MCP request") {
		require.Equal(t, "adp", request["server"])
		wantHeaders(t, request)
		if request["rpc_method"] == "tools/call" {
			calls++
		}
	}
	require.Equal(t, 1, calls, "the tool call's request is logged with its JSON-RPC method")

	failed := withMessage(records, "MCP connection failed")
	require.NotEmpty(t, failed)
	require.Equal(t, "down", failed[0]["server"])
	require.NotEmpty(t, failed[0]["error"])

	ready := withMessage(records, "MCP server ready")
	require.Len(t, ready, 1)
	require.Equal(t, "adp", ready[0]["server"])
	require.Equal(t, "acc-alice", ready[0]["namespace"])
	require.EqualValues(t, 1, ready[0]["tools"])
	unavailable := withMessage(records, "MCP server unavailable")
	require.Len(t, unavailable, 1)
	require.Equal(t, "down", unavailable[0]["server"])

	toolCalls := withMessage(records, "MCP tool call")
	require.Len(t, toolCalls, 1)
	require.Equal(t, "adp", toolCalls[0]["server"])
	require.Equal(t, "lookup", toolCalls[0]["tool"])
	require.Equal(t, "acc-alice", toolCalls[0]["namespace"])
	require.Equal(t, "call-1", toolCalls[0]["call_id"])
	wantHeaders(t, toolCalls[0])
}

// A tool that answers with an error is logged with its text, cut short.
func TestToolErrorsAreLoggedWithTheirText(t *testing.T) {
	logs := captureLogs(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "adp", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "lookup"}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "Account ID is required " + strings.Repeat("é", maxLoggedToolError)}}}, nil, nil
	})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	t.Cleanup(func() { httpServer.CloseClientConnections(); httpServer.Close() })

	client := NewClient(NewInlineStore(ServerConfig{Name: "adp", Endpoint: httpServer.URL, Transport: TransportStreamableHTTP}))
	_, tools, err := client.ListTools(t.Context(), "acc-alice", nil)
	require.NoError(t, err)
	_, err = tools[0].Execute(t.Context(), &agents.ToolCall{
		Namespace:           "acc-alice",
		FunctionCallMessage: &responses.FunctionCallMessage{Name: "lookup", Arguments: "{}"},
	})
	require.NoError(t, err)

	records, _ := logs.records(t)
	returned := withMessage(records, "MCP tool call returned an error")
	require.Len(t, returned, 1)
	text, _ := returned[0]["result"].(string)
	require.True(t, strings.HasPrefix(text, "Account ID is required"), text)
	require.True(t, strings.HasSuffix(text, "…"), "a long error is cut short")
	require.LessOrEqual(t, len(text), maxLoggedToolError+len("…"))
}

// Only a POST's body names a JSON-RPC method.
func TestRPCMethodIsReadFromACopyOfTheBody(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`
	req, err := http.NewRequest(http.MethodPost, "https://mcp.example.com/mcp", strings.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, "tools/call", rpcMethod(req))
	unread := new(bytes.Buffer)
	_, err = unread.ReadFrom(req.Body)
	require.NoError(t, err)
	require.Equal(t, body, unread.String(), "the request's own body is left to send")

	stream, _ := http.NewRequest(http.MethodGet, "https://mcp.example.com/mcp", nil)
	require.Equal(t, "", rpcMethod(stream))
	require.Equal(t, "https://mcp.example.com/mcp", loggedURL(mustURL(t, "https://user:pw@mcp.example.com/mcp?session_id=s#f")))
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}
