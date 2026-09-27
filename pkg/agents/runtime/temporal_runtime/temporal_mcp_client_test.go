package temporal_runtime

import (
	"context"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

type catalogTestClient struct {
	input  *agents.AgentInput
	called string
}
type catalogTestTool struct{ *agents.BaseTool }

func (t *catalogTestTool) Execute(context.Context, *agents.ToolCall) (*agents.ToolCallResponse, error) {
	panic("execution must use the catalog activity")
}

func (c *catalogTestClient) ListTools(_ context.Context, namespace string, runContext map[string]any, selection ...agents.MCPSelection) ([]agents.ConnectorStatus, []agents.Tool, error) {
	in := &agents.AgentInput{Namespace: namespace, RunContext: runContext}
	if len(selection) > 0 {
		in.MCP = selection[0]
	}
	c.input = in
	return []agents.ConnectorStatus{
			{Name: "mail", Kind: agents.ToolsetErrorAuth},
			{Name: "docs", Connected: true, ToolCount: 1},
			{Name: "down", Kind: agents.ToolsetErrorUnavailable, Detail: "connection refused"},
		}, []agents.Tool{&catalogTestTool{BaseTool: &agents.BaseTool{
			Name: "read", MCPServerName: "docs",
			ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{Name: "docs__read"}},
		}}}, nil
}

func (c *catalogTestClient) CallTool(_ context.Context, tool *agents.BaseTool, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
	c.called = tool.MCPServerName
	return agents.ToolCallResult(call, "read completed"), nil
}

func TestCatalogSelectionAndStatusesCrossTemporalBoundary(t *testing.T) {
	// Activities register once for the client, independently of its discovered server names.
	client := &catalogTestClient{}
	wrapper := NewTemporalMCPClient(client, nil)
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivityWithOptions(wrapper.ListTools, activity.RegisterOptions{Name: "catalog_ListMCPToolsActivity"})
	env.RegisterActivityWithOptions(wrapper.ExecuteTool, activity.RegisterOptions{Name: "catalog_ExecuteMCPToolActivity"})
	selection := agents.MCPSelection{Enable: []string{"docs"}, Disable: []string{"other"}, Tools: map[string]agents.MCPToolSelection{"docs": {Exclude: []string{"write"}}}}

	// The workflow receives auth status as data and can still execute another connector's tool.
	env.ExecuteWorkflow(func(ctx workflow.Context) ([]agents.ConnectorStatus, error) {
		ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute})
		proxy := &temporalMCPClientProxy{ctx: ctx, prefix: "catalog"}
		statuses, tools, err := proxy.ListTools(context.Background(), "alice", map[string]any{"namespace": "spoofed"}, selection)
		if err != nil {
			return nil, err
		}
		_, err = tools[0].Execute(context.Background(), &agents.ToolCall{Namespace: "alice", FunctionCallMessage: &responses.FunctionCallMessage{Name: "docs__read", CallID: "call", Arguments: "{}"}})
		return statuses, err
	})
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, "alice", client.input.Namespace)
	require.Equal(t, "spoofed", client.input.RunContext["namespace"])
	require.Equal(t, selection, client.input.MCP)
	require.Equal(t, "docs", client.called)
	var statuses []agents.ConnectorStatus
	require.NoError(t, env.GetWorkflowResult(&statuses))
	require.Equal(t, agents.ToolsetErrorAuth, statuses[0].Kind)
	require.True(t, statuses[1].Connected)
	require.Equal(t, agents.ToolsetErrorUnavailable, statuses[2].Kind)
	require.Equal(t, "connection refused", statuses[2].Detail)
}

func TestTemporalMCPProxyRejectsDirectCalls(t *testing.T) {
	// An unsupported call returns an error without attempting workflow activity execution.
	proxy := &temporalMCPClientProxy{}
	result, err := proxy.CallTool(t.Context(), nil, nil)
	require.Nil(t, result)
	require.ErrorContains(t, err, "execute a tool returned by ListTools")
}
