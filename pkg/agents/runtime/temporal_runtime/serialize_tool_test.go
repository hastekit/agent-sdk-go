package temporal_runtime_test

import (
	"context"
	"testing"

	"github.com/hastekit/agent-sdk-go/internal/testutil"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/temporal_runtime"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
)

// toolRecorder keeps the tool a middleware was shown.
type toolRecorder struct {
	agents.NoopMiddleware
	seen *agents.BaseTool
}

func (r *toolRecorder) WrapToolCall(next agents.ToolCallFunc) agents.ToolCallFunc {
	return func(ctx context.Context, tool *agents.BaseTool, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
		r.seen = tool
		return next(ctx, tool, call)
	}
}

// A function tool leaves Name empty, and the local executor fills it in before
// a middleware sees it. The activities have to show the same tool, or a policy
// keyed on the name passes locally and fails under Temporal.
func TestTemporalActivitiesShowMiddlewareTheSameToolAsLocally(t *testing.T) {
	tool := newMiddlewareTestTool("search")
	require.Empty(t, tool.GetToolDescriptor().Name, "the tool itself carries no name of its own")
	recorder := &toolRecorder{}
	a := temporal_runtime.NewTemporalAgent(nil, &agents.AgentOptions{
		Name: "A", History: newTestHistory(), Tools: []agents.Tool{tool}, MCPClient: testutil.NewMCPClient([]agents.MCPToolset{&transformToolset{tool: tool}}...),
		Middlewares: []agents.Middleware{recorder},
	}, nil)
	activities := a.GetActivities()
	mcpDescriptor := *tool.BaseTool
	mcpDescriptor.MCPServerName = "media_server"
	locally := agents.SerializeTool(tool.GetToolDescriptor(), middlewareCall())

	for name, args := range map[string][]interface{}{
		"A_search_ExecuteToolActivity": {middlewareCall()},
		"A_MCP_ExecuteMCPToolActivity": {&mcpDescriptor, middlewareCall(), map[string]any{}},
	} {
		t.Run(name, func(t *testing.T) {
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestActivityEnvironment()
			fn, ok := activities[name]
			require.True(t, ok, name)
			env.RegisterActivity(fn)
			recorder.seen = nil

			_, err := env.ExecuteActivity(fn, args...)
			require.NoError(t, err)

			require.NotNil(t, recorder.seen)
			require.Equal(t, "search", recorder.seen.Name)
			require.Equal(t, locally.Name, recorder.seen.Name)
			require.Equal(t, locally.ToolUnion.OfFunction.Name, recorder.seen.ToolUnion.OfFunction.Name)
		})
	}
	require.Empty(t, tool.GetToolDescriptor().Name, "the tool's own descriptor is left alone")
}
