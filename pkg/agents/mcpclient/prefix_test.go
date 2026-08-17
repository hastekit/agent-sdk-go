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
)

func exposedNames(tools []agents.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Tool(context.Background()).OfFunction.Name)
	}
	return names
}

// The filter, approval and deferred lists are written against the server's own
// tool names. Adding a prefix must not silently empty them out.
func TestToolPrefixKeepsUnprefixedOptionsWorking(t *testing.T) {
	srv := &MCPClient{
		ToolPrefix:            "xyz",
		ToolFilter:            []string{"search", "book"},
		ApprovalRequiredTools: []string{"book"},
		DeferredTools:         []string{"search"},
	}

	// buildLazyTools sees what fetchToolSchemas produced: prefixed names.
	tools := srv.buildLazyTools([]*mcp.Tool{
		{Name: "xyz__search"}, {Name: "xyz__book"}, {Name: "xyz__cancel"},
	}, nil, nil)

	require.Len(t, tools, 2, "the filter still selects by the server's own names")
	assert.Equal(t, []string{"xyz__search", "xyz__book"}, exposedNames(tools))

	assert.True(t, tools[0].IsDeferred(), "search was deferred by its unprefixed name")
	assert.False(t, tools[0].NeedApproval())

	assert.True(t, tools[1].NeedApproval(), "book needs approval by its unprefixed name")
	assert.False(t, tools[1].IsDeferred())
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
	}, nil, nil)

	require.Len(t, tools, 2)
	assert.Equal(t, []string{"search", "book"}, exposedNames(tools))
	assert.True(t, tools[0].IsDeferred())
	assert.True(t, tools[1].NeedApproval())
}

// The "defer everything" wildcard is not a tool name, so a prefix leaves it
// alone.
func TestToolPrefixKeepsDeferredWildcard(t *testing.T) {
	srv := &MCPClient{ToolPrefix: "xyz", DeferredTools: []string{"*"}}

	tools := srv.buildLazyTools([]*mcp.Tool{{Name: "xyz__search"}, {Name: "xyz__book"}}, nil, nil)

	require.Len(t, tools, 2)
	for i, tool := range tools {
		assert.True(t, tool.IsDeferred(), "tool %d should be deferred by the wildcard", i)
	}
}

// Cached schemas carry the prefix, so the prefix has to be part of the key.
func TestSchemaCacheKeyIncludesToolPrefix(t *testing.T) {
	a := &MCPClient{Endpoint: "https://example.test/mcp", Transport: "streamable-http", ToolPrefix: "a"}
	b := &MCPClient{Endpoint: "https://example.test/mcp", Transport: "streamable-http", ToolPrefix: "b"}
	none := &MCPClient{Endpoint: "https://example.test/mcp", Transport: "streamable-http"}

	assert.NotEqual(t, a.schemaCacheKey(nil), b.schemaCacheKey(nil))
	assert.NotEqual(t, a.schemaCacheKey(nil), none.schemaCacheKey(nil))
}

// memCache is a SchemaCache standing in for the Redis-backed one a multi-pod
// deployment would inject.
type memCache struct {
	mu      sync.Mutex
	entries map[string]*CachedToolEntry
}

func newMemCache() *memCache { return &memCache{entries: map[string]*CachedToolEntry{}} }

func (m *memCache) Get(_ context.Context, key string) (*CachedToolEntry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key]
	return e, ok
}

func (m *memCache) Set(_ context.Context, key string, entry *CachedToolEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[key] = entry
}

func (m *memCache) Delete(_ context.Context, key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, key)
}

func (m *memCache) Clear(_ context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = map[string]*CachedToolEntry{}
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
	client, err := NewClient(ctx, url,
		WithTransport("streamable-http"),
		WithToolPrefix("xyz"),
	)
	require.NoError(t, err)

	// The model-facing path: whatever ListTools advertised is what comes back as
	// the call's name.
	tools, err := client.ListTools(ctx, nil)
	require.NoError(t, err)
	require.Len(t, tools, 1)
	require.Equal(t, "xyz__echo", tools[0].Tool(ctx).OfFunction.Name)

	res, err := tools[0].Execute(ctx, echoCall("xyz__echo"))
	require.NoError(t, err)
	require.NotNil(t, res.FunctionCallOutputMessage)
	assert.Equal(t, "called as echo", *res.FunctionCallOutputMessage.Output.OfString)

	// The durable-runtime path: only a serialized tool call crosses the boundary,
	// so the prefix has to come off here too.
	res, err = client.CallToolDirect(ctx, nil, echoCall("xyz__echo"))
	require.NoError(t, err)
	require.NotNil(t, res.FunctionCallOutputMessage)
	assert.Equal(t, "called as echo", *res.FunctionCallOutputMessage.Output.OfString)

	assert.Equal(t, []string{"echo", "echo"}, namesSeen(),
		"the server should never see the prefix")
}

// Prefixed names are what get cached, so two clients sharing a cache over one
// endpoint must each still see their own prefix rather than the other's.
func TestToolPrefixIsNotSharedThroughSchemaCache(t *testing.T) {
	url, namesSeen := echoServer(t)
	cache := newMemCache()

	ctx := context.Background()
	newPrefixed := func(prefix string) *MCPClient {
		opts := []McpServerOption{WithTransport("streamable-http"), WithSchemaCache(cache)}
		if prefix != "" {
			opts = append(opts, WithToolPrefix(prefix))
		}
		client, err := NewClient(ctx, url, opts...)
		require.NoError(t, err)
		return client
	}

	for _, prefix := range []string{"alpha", "beta", ""} {
		client := newPrefixed(prefix)

		tools, err := client.ListTools(ctx, nil)
		require.NoError(t, err)
		require.Len(t, tools, 1)

		want := "echo"
		if prefix != "" {
			want = prefix + "__echo"
		}
		require.Equal(t, want, tools[0].Tool(ctx).OfFunction.Name,
			"prefix %q read the wrong name out of the shared cache", prefix)

		// A second pass goes through the cache rather than the wire — the name
		// must not pick up the prefix twice.
		tools, err = client.ListTools(ctx, nil)
		require.NoError(t, err)
		require.Len(t, tools, 1)
		require.Equal(t, want, tools[0].Tool(ctx).OfFunction.Name, "cached name changed on re-read")

		res, err := tools[0].Execute(ctx, echoCall(want))
		require.NoError(t, err)
		assert.Equal(t, "called as echo", *res.FunctionCallOutputMessage.Output.OfString)
	}

	assert.Equal(t, []string{"echo", "echo", "echo"}, namesSeen())
}
