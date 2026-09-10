package temporal_runtime_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/temporal_runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// abortingMiddleware fails before it would call next, which ends the run.
type abortingMiddleware struct {
	agents.NoopMiddleware
	err error
}

func (h *abortingMiddleware) WrapToolCall(next agents.ToolCallFunc) agents.ToolCallFunc {
	return func(ctx context.Context, tool *agents.BaseTool, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
		if h.err != nil {
			return nil, h.err
		}
		return next(ctx, tool, call)
	}
}

// The tool's activity turns a middleware's error into a failure Temporal will not
// retry. No RetryPolicy is set anywhere, so the server default of unlimited
// attempts applies, and without this the workflow would keep re-running a wrap
// that has already said no instead of ending on it.
func TestToolActivity_MakesAMiddlewareErrorNonRetryable(t *testing.T) {
	tool := newMiddlewareTestTool("search")
	agent := temporal_runtime.NewTemporalAgent(nil, &agents.AgentOptions{
		Name:        "Agent",
		History:     newTestHistory(),
		Tools:       []agents.Tool{tool},
		Middlewares: []agents.Middleware{&abortingMiddleware{err: errors.New("tenant mismatch")}},
	}, nil)

	fn, ok := agent.GetActivities()["Agent_search_ExecuteToolActivity"].(func(context.Context, *agents.ToolCall) (*agents.ToolCallResponse, error))
	require.True(t, ok, "the tool activity is registered with the expected signature")

	_, err := fn(context.Background(), middlewareCall())
	require.Error(t, err)
	assert.False(t, tool.ran, "the tool never ran")

	assert.True(t, temporal_runtime.WasAborted(err), "a worker with its own activities can still classify it")

	var appErr *temporal.ApplicationError
	require.True(t, errors.As(err, &appErr))
	assert.True(t, appErr.NonRetryable(), "a middleware that said no must not be re-asked")
	assert.Contains(t, err.Error(), "tenant mismatch")
}

// A middleware that returns no error leaves the activity alone, so the ordinary path
// is untouched.
func TestToolActivity_LeavesASuccessfulMiddlewareAlone(t *testing.T) {
	tool := newMiddlewareTestTool("search")
	agent := temporal_runtime.NewTemporalAgent(nil, &agents.AgentOptions{
		Name:        "Agent",
		History:     newTestHistory(),
		Tools:       []agents.Tool{tool},
		Middlewares: []agents.Middleware{&abortingMiddleware{}},
	}, nil)

	fn, ok := agent.GetActivities()["Agent_search_ExecuteToolActivity"].(func(context.Context, *agents.ToolCall) (*agents.ToolCallResponse, error))
	require.True(t, ok)

	res, err := fn(context.Background(), middlewareCall())
	require.NoError(t, err)
	assert.Equal(t, "tool ran", *res.Output.OfString)
	assert.True(t, tool.ran)
}

// Inside the workflow, a tool activity that failed on a middleware's error reaches
// the executor as an abort — recognised by its type, which is what survives a
// boundary that rewrites errors.
func TestTemporalToolExecutor_MiddlewareFailureReachesTheExecutorAsAnAbort(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	env.RegisterActivityWithOptions(
		func(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
			// What the tool activity produces for a middleware error.
			return nil, temporal.NewNonRetryableApplicationError(
				"tenant mismatch", temporal_runtime.ToolCallAbortedErrorType, nil)
		}, activityNamed("Agent_worker_ExecuteToolActivity"))

	env.RegisterWorkflow(abortProxyWorkflow)
	env.ExecuteWorkflow(abortProxyWorkflow)

	require.True(t, env.IsWorkflowCompleted())

	var aborted bool
	require.NoError(t, env.GetWorkflowResult(&aborted))
	assert.True(t, aborted, "the executor's error is recognised as an abort inside the workflow")
}

// abortProxyWorkflow drives the executor the way the agent loop would and
// reports whether the middleware's failure arrived as an abort.
func abortProxyWorkflow(ctx workflow.Context) (bool, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: testActivityTimeout,
	})

	executor := temporal_runtime.NewTemporalToolExecutor(ctx)
	results := executor.ExecuteAll(context.Background(), []agents.ExecutableToolCall{{
		ToolName: "worker",
		Tool:     temporal_runtime.NewTemporalToolProxy(ctx, "Agent_worker", nil),
		ToolCall: middlewareCall(),
	}})
	return agents.IsToolCallAborted(results[0].Err), nil
}
