package temporal_runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/temporal_runtime"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

const testActivityTimeout = 10 * time.Second

func activityNamed(name string) activity.RegisterOptions {
	return activity.RegisterOptions{Name: name}
}

func newTestHistory() *history.CommonConversationManager {
	return history.NewConversationManager(history.NewInMemoryConversationPersistence())
}

// middlewareBaseTool is the tool a middleware is shown. It carries a ToolUnion because that
// is what crosses to the activity: a BaseTool with an empty union cannot be
// encoded at all.
func middlewareBaseTool() *agents.BaseTool {
	return &agents.BaseTool{
		Name: "search",
		ToolUnion: responses.ToolUnion{
			OfFunction: &responses.FunctionTool{Name: "xyz__search"},
		},
	}
}

func middlewareCall() *agents.ToolCall {
	return &agents.ToolCall{FunctionCallMessage: &responses.FunctionCallMessage{
		ID: "fc_1", CallID: "call_1", Name: "search",
	}}
}

// middlewareTestTool is a tool that records whether it ran.
type middlewareTestTool struct {
	*agents.BaseTool
	ran bool
}

func newMiddlewareTestTool(name string) *middlewareTestTool {
	return &middlewareTestTool{BaseTool: &agents.BaseTool{ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{
		Name:        name,
		Description: utils.Ptr("test tool"),
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
	}}}}
}

func (t *middlewareTestTool) Execute(_ context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
	t.ran = true
	return agents.ToolCallResult(call, "tool ran"), nil
}

// authzMiddleware refuses every call it wraps, without running the tool.
type authzMiddleware struct {
	agents.NoopMiddleware
	name string
}

func (h *authzMiddleware) WrapToolCall(agents.ToolCallFunc) agents.ToolCallFunc {
	return func(_ context.Context, _ *agents.BaseTool, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
		return agents.ToolCallResult(call, "denied by "+h.name), nil
	}
}

// Middlewares register no activities of their own: their wraps run inside the tool
// and LLM activities, and the workflow never calls them.
func TestMiddlewares_RegisterNoActivitiesOfTheirOwn(t *testing.T) {
	agent := temporal_runtime.NewTemporalAgent(nil, &agents.AgentOptions{
		Name:        "Agent",
		History:     newTestHistory(),
		Tools:       []agents.Tool{newMiddlewareTestTool("search")},
		Middlewares: []agents.Middleware{&authzMiddleware{name: "authz"}},
	}, nil)

	for name := range agent.GetActivities() {
		assert.NotContains(t, name, "Middleware")
		assert.NotContains(t, name, "Wrap")
	}
}

// The tool's activity runs the real middlewares on the worker, so what it returns to
// the workflow is what the chain made of the call — here a refusal, with the
// tool never run.
func TestToolActivity_RunsTheMiddlewaresWrapOnTheWorker(t *testing.T) {
	tool := newMiddlewareTestTool("search")
	agent := temporal_runtime.NewTemporalAgent(nil, &agents.AgentOptions{
		Name:        "Agent",
		History:     newTestHistory(),
		Tools:       []agents.Tool{tool},
		Middlewares: []agents.Middleware{&authzMiddleware{name: "authz"}},
	}, nil)

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	fn := agent.GetActivities()["Agent_search_ExecuteToolActivity"]
	env.RegisterActivity(fn)

	val, err := env.ExecuteActivity(fn, middlewareCall())
	require.NoError(t, err)

	var out *agents.ToolCallResponse
	require.NoError(t, val.Get(&out))
	require.NotNil(t, out)
	assert.Equal(t, "denied by authz", *out.Output.OfString)
	assert.False(t, tool.ran, "a refused tool must not run")
}

// The workflow-side executor holds no middlewares: it runs the tool's activity and
// hands back whatever the worker returned.
func TestTemporalToolExecutor_ReturnsTheActivitysResult(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	env.RegisterActivityWithOptions(
		func(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
			return agents.ToolCallResult(call, "tool ran"), nil
		}, activityNamed("Agent_worker_ExecuteToolActivity"))

	env.RegisterWorkflow(toolProxyWorkflow)
	env.ExecuteWorkflow(toolProxyWorkflow)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var out string
	require.NoError(t, env.GetWorkflowResult(&out))
	assert.Equal(t, "tool ran", out)
}

// toolProxyWorkflow drives the executor the way the agent loop would.
func toolProxyWorkflow(ctx workflow.Context) (string, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: testActivityTimeout,
	})

	executor := temporal_runtime.NewTemporalToolExecutor(ctx)
	results := executor.ExecuteAll(context.Background(), []agents.ExecutableToolCall{{
		ToolName: "worker",
		Tool:     temporal_runtime.NewTemporalToolProxy(ctx, "Agent_worker", nil),
		ToolCall: middlewareCall(),
	}})
	if results[0].Err != nil {
		return "", results[0].Err
	}
	return *results[0].Response.Output.OfString, nil
}
