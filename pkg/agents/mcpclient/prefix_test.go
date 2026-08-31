package mcpclient

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func exposedNames(tools []agents.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.GetToolDescriptor().ToolUnion.OfFunction.Name)
	}
	return names
}

// The prefix is used verbatim, so the separator lives in what the caller passes.
func TestToolPrefixIsUsedVerbatim(t *testing.T) {
	assert.Equal(t, "xyz__search", PrefixedToolName("xyz__", "search"))
	assert.Equal(t, "search", PrefixedToolName("", "search"))
}

// The model sees the prefixed name; the tool keeps the server's own name, which
// is what the call is finally made under.
func TestToolPrefixKeepsBothNames(t *testing.T) {
	srv := &MCPClient{ToolPrefix: "xyz__"}

	tools := srv.buildLazyTools([]*mcp.Tool{{Name: "search"}, {Name: "book"}}, nil, serverConn{})

	require.Len(t, tools, 2)
	assert.Equal(t, []string{"xyz__search", "xyz__book"}, exposedNames(tools))

	assert.Equal(t, "search", tools[0].GetToolDescriptor().Name, "the server's own name is kept alongside")
}

// The filter, approval and deferred lists are written against the server's own
// tool names. Adding a prefix must not silently empty them out.
func TestToolPrefixKeepsUnprefixedOptionsWorking(t *testing.T) {
	srv := &MCPClient{
		ToolPrefix:            "xyz__",
		ToolFilter:            []string{"search", "book"},
		ApprovalRequiredTools: []string{"book"},
		DeferredTools:         []string{"search"},
	}

	tools := srv.buildLazyTools([]*mcp.Tool{
		{Name: "search"}, {Name: "book"}, {Name: "cancel"},
	}, nil, serverConn{})

	require.Len(t, tools, 2, "the filter selects by the server's own names")
	assert.Equal(t, []string{"xyz__search", "xyz__book"}, exposedNames(tools))

	assert.True(t, tools[0].GetToolDescriptor().Deferred, "search was deferred by its unprefixed name")
	assert.False(t, tools[0].GetToolDescriptor().RequiresApproval)

	assert.True(t, tools[1].GetToolDescriptor().RequiresApproval, "book needs approval by its unprefixed name")
	assert.False(t, tools[1].GetToolDescriptor().Deferred)
}

// The same lists against a client with no prefix — the path every existing
// caller is on.
func TestOptionsWithoutToolPrefix(t *testing.T) {
	srv := &MCPClient{
		ToolFilter:            []string{"search", "book"},
		ApprovalRequiredTools: []string{"book"},
		DeferredTools:         []string{"search"},
	}

	tools := srv.buildLazyTools([]*mcp.Tool{
		{Name: "search"}, {Name: "book"}, {Name: "cancel"},
	}, nil, serverConn{})

	require.Len(t, tools, 2)
	assert.Equal(t, []string{"search", "book"}, exposedNames(tools))
	assert.True(t, tools[0].GetToolDescriptor().Deferred)
	assert.True(t, tools[1].GetToolDescriptor().RequiresApproval)
}

// The "defer everything" wildcard is not a tool name, so a prefix leaves it
// alone.
func TestToolPrefixKeepsDeferredWildcard(t *testing.T) {
	srv := &MCPClient{ToolPrefix: "xyz__", DeferredTools: []string{"*"}}

	tools := srv.buildLazyTools([]*mcp.Tool{{Name: "search"}, {Name: "book"}}, nil, serverConn{})

	require.Len(t, tools, 2)
	for i, tool := range tools {
		assert.True(t, tool.GetToolDescriptor().Deferred, "tool %d should be deferred by the wildcard", i)
	}
}

// memCache is a SchemaCache standing in for the Redis-backed one a multi-pod
// deployment would inject.
type memCache struct {
	mu      sync.Mutex
	entries map[string]*CachedToolEntry
	ttls    map[string]time.Duration
}

func newMemCache() *memCache {
	return &memCache{entries: map[string]*CachedToolEntry{}, ttls: map[string]time.Duration{}}
}

func (m *memCache) Get(_ context.Context, key string) (*CachedToolEntry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key]
	return e, ok
}

func (m *memCache) Set(_ context.Context, key string, entry *CachedToolEntry, ttl time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[key] = entry
	m.ttls[key] = ttl
}

func (m *memCache) Delete(_ context.Context, key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, key)
	delete(m.ttls, key)
}

// keys returns what is stored, so a test can see which key a listing landed
// under and not just how many there are.
func (m *memCache) keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Sorted(maps.Keys(m.entries))
}

func (m *memCache) ttl(key string) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ttls[key]
}

func (m *memCache) Clear(_ context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = map[string]*CachedToolEntry{}
	m.ttls = map[string]time.Duration{}
}

func (m *memCache) size() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}

// echoServer serves one "echo" tool and records the name each call arrived
// under, which is how we see what actually reached the server.
func echoServer(t *testing.T) (url string, namesSeen func() []string) {
	t.Helper()

	var mu sync.Mutex
	var seen []string

	server := mcp.NewServer(&mcp.Implementation{Name: "echo-server", Version: "0.1.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "echo",
		Description: "Echoes back the name it was called under.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, any, error) {
		mu.Lock()
		seen = append(seen, req.Params.Name)
		mu.Unlock()
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "called as " + req.Params.Name}},
		}, nil, nil
	})

	httpSrv := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server }, nil,
	))
	// The pool keeps its session on context.Background() by design, so a plain
	// Close would block forever waiting on that connection.
	t.Cleanup(func() {
		httpSrv.CloseClientConnections()
		httpSrv.Close()
	})

	return httpSrv.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func echoCall(name string) *agents.ToolCall {
	return &agents.ToolCall{
		FunctionCallMessage: &responses.FunctionCallMessage{
			ID:        "fc_echo",
			CallID:    "call_echo",
			Name:      name,
			Arguments: "{}",
		},
	}
}

// The prefix is presentation only: the model calls "xyz__echo", and the server —
// which has never heard of the prefix — must still be asked for "echo".
func TestToolPrefixCallsServerUnderItsOwnName(t *testing.T) {
	url, namesSeen := echoServer(t)

	ctx := context.Background()
	client, err := NewClient(ctx, "prefix", url,
		WithTransport("streamable-http"),
		WithToolPrefix("xyz__"),
	)
	require.NoError(t, err)

	// The model-facing path: whatever ListTools advertised is what comes back as
	// the call's name.
	tools, err := client.ListTools(ctx, nil)
	require.NoError(t, err)
	require.Len(t, tools, 1)
	require.Equal(t, "xyz__echo", tools[0].GetToolDescriptor().ToolUnion.OfFunction.Name)

	res, err := tools[0].Execute(ctx, echoCall("xyz__echo"))
	require.NoError(t, err)
	require.NotNil(t, res.FunctionCallOutputMessage)
	assert.Equal(t, "called as echo", *res.FunctionCallOutputMessage.Output.OfString)

	// The durable-runtime path: the tool crosses the boundary as data alongside
	// the call, so its own name is already in hand and nothing is resolved here.
	res, err = client.CallToolDirect(ctx, nil, tools[0].GetToolDescriptor(), echoCall("xyz__echo"))
	require.NoError(t, err)
	require.NotNil(t, res.FunctionCallOutputMessage)
	assert.Equal(t, "called as echo", *res.FunctionCallOutputMessage.Output.OfString)

	assert.Equal(t, []string{"echo", "echo"}, namesSeen(),
		"the server should never see the prefix")
}

// Cached schemas are the server's own, so one entry serves every prefix — which
// is why the prefix is not part of the cache key.
func TestSchemaCacheIsSharedAcrossPrefixes(t *testing.T) {
	url, namesSeen := echoServer(t)
	cache := newMemCache()

	ctx := context.Background()
	newPrefixed := func(prefix string) *MCPClient {
		opts := []McpServerOption{WithTransport("streamable-http"), WithSchemaCache(cache)}
		if prefix != "" {
			opts = append(opts, WithToolPrefix(prefix))
		}
		client, err := NewClient(ctx, "prefix", url, opts...)
		require.NoError(t, err)
		return client
	}

	for _, prefix := range []string{"alpha__", "beta__", ""} {
		client := newPrefixed(prefix)

		for pass := range 2 { // the second pass reads the cache
			tools, err := client.ListTools(ctx, nil)
			require.NoError(t, err)
			require.Len(t, tools, 1)

			assert.Equal(t, prefix+"echo", tools[0].GetToolDescriptor().ToolUnion.OfFunction.Name,
				"prefix %q, pass %d", prefix, pass)

			res, err := tools[0].Execute(ctx, echoCall(prefix+"echo"))
			require.NoError(t, err)
			assert.Equal(t, "called as echo", *res.FunctionCallOutputMessage.Output.OfString)
		}
	}

	assert.Equal(t, 1, cache.size(),
		"raw schemas cache once and every prefix reads the same entry")
	assert.Equal(t, []string{"echo", "echo", "echo", "echo", "echo", "echo"}, namesSeen())
}

// CallToolDirect runs the tool it is handed, not a name it resolves. Refusing
// without one is what keeps it from reaching a tool the filter excluded — a tool
// that was never listed has no BaseTool to pass.
func TestCallToolDirectNeedsTheTool(t *testing.T) {
	url, namesSeen := echoServer(t)

	ctx := context.Background()
	client, err := NewClient(ctx, "prefix", url, WithTransport("streamable-http"), WithToolPrefix("xyz__"))
	require.NoError(t, err)

	_, err = client.CallToolDirect(ctx, nil, nil, echoCall("xyz__echo"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "xyz__echo")

	_, err = client.CallToolDirect(ctx, nil, &agents.BaseTool{}, echoCall("xyz__echo"))
	require.Error(t, err, "a tool with no name of its own is no better than none")

	assert.Empty(t, namesSeen(), "nothing reached the server")
}
