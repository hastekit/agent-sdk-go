package restate_runtime

import (
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/mcpclient"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

// Exercise the body of the Restate run step without a live Restate deployment.
func TestMCPMetaTemplatesInsideStepBody(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "meta", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo"}, func(_ context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		data, err := json.Marshal(req.Params.Meta)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}, nil, err
	})
	hs := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	t.Cleanup(func() { hs.CloseClientConnections(); hs.Close() })
	client, err := mcpclient.NewClient(t.Context(), t.Name(), hs.URL, mcpclient.WithTransport(mcpclient.TransportStreamableHTTP), mcpclient.WithMeta(map[string]any{"thread_id": "{{thread_id}}", "run_id": "{{run_id}}"}))
	require.NoError(t, err)
	tool := NewRestateMCPTool(nil, client, map[string]any{"run_id": "run", "thread_id": "thread"}, agents.BaseTool{Name: "echo"}, nil)
	result, err := tool.execute(t.Context(), &agents.ToolCall{
		ThreadID: "thread", FunctionCallMessage: &responses.FunctionCallMessage{Name: "echo", Arguments: "{}"},
	})
	require.NoError(t, err)
	require.Contains(t, *result.Output.OfString, `"thread_id":"thread"`)
	require.Contains(t, *result.Output.OfString, `"run_id":"run"`)
}
