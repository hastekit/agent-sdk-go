package temporal_runtime_test

import (
	"context"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/temporal_runtime"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// The tool crosses into the activity with the call, so the far side never has to
// resolve one by name — the name it would have is the model-facing one, which is
// not what the MCP server answers to.
func TestTemporalMCPToolProxy_SendsTheToolWithTheCall(t *testing.T) {
	var gotTool *agents.BaseTool
	var gotCall *agents.ToolCall

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	env.RegisterActivityWithOptions(
		func(ctx context.Context, tool *agents.BaseTool, call *agents.ToolCall, runContext map[string]any) (*agents.ToolCallResponse, error) {
			gotTool, gotCall = tool, call
			return agents.ToolCallResult(call, "tool ran"), nil
		}, activityNamed("server_ExecuteMCPToolActivity"))

	env.RegisterWorkflow(mcpProxyWorkflow)
	env.ExecuteWorkflow(mcpProxyWorkflow)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	require.NotNil(t, gotTool)
	assert.Equal(t, "echo", gotTool.Name, "the server's own name, ready to call with")
	assert.Equal(t, "xyz__echo", gotTool.ToolUnion.OfFunction.Name, "and the model-facing one")
	assert.Equal(t, "search-server", gotTool.Meta["server_name"], "meta rides along too")

	require.NotNil(t, gotCall)
	assert.Equal(t, "xyz__echo", gotCall.Name, "the call still names the tool as the model did")
}

func mcpProxyWorkflow(ctx workflow.Context) (string, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: testActivityTimeout,
	})

	proxy := temporal_runtime.NewTemporalMCPToolProxy(ctx, "server", nil, agents.BaseTool{
		Name: "echo",
		Meta: map[string]any{"server_name": "search-server"},
		ToolUnion: responses.ToolUnion{
			OfFunction: &responses.FunctionTool{Name: "xyz__echo"},
		},
	})

	resp, err := proxy.Execute(context.Background(), &agents.ToolCall{
		FunctionCallMessage: &responses.FunctionCallMessage{
			ID: "fc_1", CallID: "call_1", Name: "xyz__echo", Arguments: "{}",
		},
	})
	if err != nil {
		return "", err
	}
	return *resp.Output.OfString, nil
}
