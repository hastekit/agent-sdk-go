package restate_runtime

import (
	"context"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
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
// a middleware sees it. The run steps have to show the same tool, or a policy
// keyed on the name passes locally and fails under Restate.
func TestRestateStepsShowMiddlewareTheSameToolAsLocally(t *testing.T) {
	tool := &transformMediaTool{BaseTool: &agents.BaseTool{ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{Name: "media"}}}}
	require.Empty(t, tool.GetToolDescriptor().Name, "the tool itself carries no name of its own")
	recorder := &toolRecorder{}
	call := &agents.ToolCall{FunctionCallMessage: &responses.FunctionCallMessage{CallID: "call", Name: "media"}, Namespace: "tenant"}
	locally := agents.SerializeTool(tool.GetToolDescriptor(), call)

	local := NewRestateTool(nil, tool, nil, recorder)
	mcp := NewRestateMCPTool(nil, &transformMediaToolset{tool: tool}, nil, *tool.BaseTool, nil, recorder)
	for _, execute := range []func(context.Context, *agents.ToolCall) (*agents.ToolCallResponse, error){local.execute, mcp.execute} {
		recorder.seen = nil

		_, err := execute(t.Context(), call)
		require.NoError(t, err)

		require.NotNil(t, recorder.seen)
		require.Equal(t, "media", recorder.seen.Name)
		require.Equal(t, locally.Name, recorder.seen.Name)
		require.Equal(t, locally.ToolUnion.OfFunction.Name, recorder.seen.ToolUnion.OfFunction.Name)
	}
	require.Empty(t, tool.GetToolDescriptor().Name, "the tool's own descriptor is left alone")
}
