package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestMetaTemplatesCachedAndDirectCalls(t *testing.T) {
	var lists atomic.Int32
	server := mcp.NewServer(&mcp.Implementation{Name: "metadata", Version: "1"}, &mcp.ServerOptions{
		SetCacheable: func(_ context.Context, _ mcp.Request, c *mcp.Cacheable) { c.TTLMs = 60000 },
	})
	mcp.AddTool(server, &mcp.Tool{Name: "echo"}, func(_ context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		data, err := json.Marshal(req.Params.Meta)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}, nil, err
	})
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/list" {
				lists.Add(1)
			}
			return next(ctx, method, req)
		}
	})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	t.Cleanup(func() { httpServer.CloseClientConnections(); httpServer.Close() })
	cache := newMemCache()
	client, err := NewClient(t.Context(), t.Name(), httpServer.URL,
		WithTransport(TransportStreamableHTTP), WithSchemaCache(cache), WithCacheTTL(time.Minute),
		WithMeta(map[string]any{"static": "kept", "thread_id": "{{thread_id}}", "run_id": "{{run_id}}", "progressToken": "configured-token"}))
	require.NoError(t, err)
	conn, err := client.connFor(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { globalPool.Remove(conn) })
	var pooled *mcp.ClientSession
	for i := range 3 {
		rc := map[string]any{"run_id": fmt.Sprintf("run-%d", i), "thread_id": fmt.Sprintf("thread-%d", i)}
		tools, err := client.ListTools(t.Context(), rc)
		require.NoError(t, err)
		require.Len(t, tools, 1)
		descriptorJSON, err := json.Marshal(tools[0].GetToolDescriptor())
		require.NoError(t, err)
		require.NotContains(t, string(descriptorJSON), rc["thread_id"])
		call := echoCall("echo")
		call.ThreadID = fmt.Sprintf("thread-%d", i)
		call.Progress = discardMetaProgress{}
		var result *agents.ToolCallResponse
		if i == 2 {
			// Match durable runtimes: only a serialized descriptor and call data cross.
			var descriptor agents.BaseTool
			require.NoError(t, json.Unmarshal(descriptorJSON, &descriptor))
			result, err = client.CallToolDirect(t.Context(), rc, &descriptor, call)
		} else {
			call.RunContext = rc
			result, err = tools[0].Execute(t.Context(), call)
		}
		require.NoError(t, err)
		encoded, err := json.Marshal(result.Output)
		require.NoError(t, err)
		require.Contains(t, string(encoded), call.ThreadID)
		require.Contains(t, string(encoded), rc["run_id"])
		require.Contains(t, string(encoded), "kept")
		require.Contains(t, string(encoded), call.CallID)
		require.NotContains(t, string(encoded), "configured-token")
		session, release, err := checkoutSession(t.Context(), conn)
		require.NoError(t, err)
		if pooled != nil {
			require.Same(t, pooled, session, "metadata must not fragment the connection pool")
		}
		pooled = session
		release()
	}
	require.Equal(t, int32(1), lists.Load())
	require.Equal(t, 1, cache.size())
	for _, key := range cache.keys() {
		data, _, err := cache.Get(t.Context(), key)
		require.NoError(t, err)
		require.NotContains(t, string(data), "thread_id")
		require.NotContains(t, string(data), "run_id")
	}
	require.Equal(t, "{{thread_id}}", client.Meta["thread_id"])

	// Simultaneous executions must never see another call's context.
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			call := echoCall("echo")
			call.ThreadID = fmt.Sprintf("concurrent-thread-%d", i)
			runID := fmt.Sprintf("concurrent-run-%d", i)
			result, err := client.CallToolDirect(t.Context(), map[string]any{"run_id": runID, "thread_id": call.ThreadID}, &agents.BaseTool{Name: "echo"}, call)
			require.NoError(t, err)
			require.NotNil(t, result.Output.OfString)
			var received map[string]any
			require.NoError(t, json.Unmarshal([]byte(*result.Output.OfString), &received))
			require.Equal(t, call.ThreadID, received["thread_id"])
			require.Equal(t, runID, received["run_id"])
		})
	}
	wg.Wait()
}

type discardMetaProgress struct{}

func (discardMetaProgress) Report(context.Context, agents.ToolProgress) {}

func TestMetaTemplatesNestedValues(t *testing.T) {
	base := mcp.Meta{
		"nested":     map[string]any{"id": "prefix-{{run_id}}", "count": 4, "enabled": true, "null": nil},
		"array":      []any{"{{run_id}}", map[string]any{"id": "{{thread_id}}"}},
		"strings":    []string{"{{run_id}}", "literal"},
		"string_map": map[string]string{"id": "{{run_id}}"},
	}
	got := resolveMeta(base, map[string]any{"run_id": "run-1", "thread_id": "thread-1"})
	require.Equal(t, map[string]any{"id": "prefix-run-1", "count": 4, "enabled": true, "null": nil}, got["nested"])
	require.Equal(t, []any{"run-1", map[string]any{"id": "thread-1"}}, got["array"])
	require.Equal(t, []string{"run-1", "literal"}, got["strings"])
	require.Equal(t, map[string]string{"id": "run-1"}, got["string_map"])
	require.Equal(t, "prefix-{{run_id}}", base["nested"].(map[string]any)["id"])
	require.Equal(t, "{{run_id}}", base["array"].([]any)[0])
	require.Equal(t, "prefix-run-2", resolveMeta(base, map[string]any{"run_id": "run-2"})["nested"].(map[string]any)["id"])
	require.Nil(t, resolveMeta(nil, nil))
}

func TestCachedSchemasUseCurrentMetaTemplates(t *testing.T) {
	url, lists := cacheableServer(t, time.Minute, CacheScopePublic)
	cache := newMemCache()
	for _, template := range []string{"first-{{run_id}}", "second-{{run_id}}"} {
		client := cachingClient(t, url, cache, WithMeta(map[string]any{"id": template}))
		tools, err := client.ListTools(t.Context(), nil)
		require.NoError(t, err)
		require.Equal(t, template, tools[0].(*LazyMcpTool).meta["id"])
	}
	require.Equal(t, 1, lists())
}
