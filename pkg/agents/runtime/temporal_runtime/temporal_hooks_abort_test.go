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

// abortingHook fails from its before phase, which ends the run.
type abortingHook struct {
	agents.NoopModelCallHook

	name    string
	err     error
	befores *int
}

func (h *abortingHook) GetName() string { return h.name }

func (h *abortingHook) BeforeToolCall(ctx context.Context, call *agents.ToolCall) (agents.ToolCallHookResult, error) {
	if h.befores != nil {
		*h.befores++
	}
	return agents.ContinueToolCall(), h.err
}

func (h *abortingHook) AfterToolCall(ctx context.Context, call *agents.ToolCall, result *agents.ToolCallResponse) (agents.ToolCallHookResult, error) {
	return agents.ContinueToolCall(), nil
}

// The registered activity — not the hook itself — is what keeps Temporal from
// retrying. No RetryPolicy is set anywhere, so the server default of unlimited
// attempts applies, and without this the workflow would keep re-asking a hook
// that has already said no instead of ending on it.
func TestHookActivity_MakesAHookErrorNonRetryable(t *testing.T) {
	befores := 0
	agent := temporal_runtime.NewTemporalAgent(nil, &agents.AgentOptions{
		Name:    "Agent",
		History: newTestHistory(),
		Hooks:   []agents.Hook{&abortingHook{name: "authz", err: errors.New("tenant mismatch"), befores: &befores}},
	}, nil)

	fn, ok := agent.GetActivities()["Agent_authz_BeforeToolCallActivity"].(func(context.Context, *agents.ToolCall) (agents.ToolCallHookResult, error))
	require.True(t, ok, "the before-tool-call activity is registered with the wrapped signature")

	_, err := fn(context.Background(), hookCall())
	require.Error(t, err)
	assert.Equal(t, 1, befores, "the real hook ran")

	assert.True(t, temporal_runtime.WasAborted(err), "a worker with its own activities can still classify it")

	var appErr *temporal.ApplicationError
	require.True(t, errors.As(err, &appErr))
	assert.True(t, appErr.NonRetryable(), "a hook that said no must not be re-asked")
	assert.Contains(t, err.Error(), "tenant mismatch")
}

// A hook that returns no error leaves the activity alone, so the ordinary path
// is untouched.
func TestHookActivity_LeavesASuccessfulHookAlone(t *testing.T) {
	agent := temporal_runtime.NewTemporalAgent(nil, &agents.AgentOptions{
		Name:    "Agent",
		History: newTestHistory(),
		Hooks:   []agents.Hook{&abortingHook{name: "authz"}},
	}, nil)

	fn, ok := agent.GetActivities()["Agent_authz_BeforeToolCallActivity"].(func(context.Context, *agents.ToolCall) (agents.ToolCallHookResult, error))
	require.True(t, ok)

	res, err := fn(context.Background(), hookCall())
	require.NoError(t, err)
	assert.False(t, res.Handled, "the call carries on to the tool")
}

// Inside the workflow, a failed hook activity reaches the executor as an abort —
// recognised by where it came from rather than by its type, which is what lets
// the mark survive a boundary that rewrites errors.
func TestTemporalHookProxy_HookFailureReachesTheExecutorAsAnAbort(t *testing.T) {
	var ran []string

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	env.RegisterActivityWithOptions(
		func(ctx context.Context, call *agents.ToolCall) (agents.ToolCallHookResult, error) {
			ran = append(ran, "before:authz")
			// What the registered wrapper produces for a hook error.
			return agents.ContinueToolCall(), temporal.NewNonRetryableApplicationError(
				"tenant mismatch", temporal_runtime.ToolCallAbortedErrorType, nil)
		}, activityNamed("authz_BeforeToolCallActivity"))
	env.RegisterActivityWithOptions(
		func(ctx context.Context, call *agents.ToolCall, result *agents.ToolCallResponse) (agents.ToolCallHookResult, error) {
			ran = append(ran, "after:authz")
			return agents.ContinueToolCall(), nil
		}, activityNamed("authz_AfterToolCallActivity"))
	env.RegisterActivityWithOptions(
		func(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
			ran = append(ran, "exec")
			return agents.ToolCallResult(call, "tool ran"), nil
		}, activityNamed("Agent_worker_ExecuteToolActivity"))

	env.RegisterWorkflow(abortProxyWorkflow)
	env.ExecuteWorkflow(abortProxyWorkflow)

	require.True(t, env.IsWorkflowCompleted())

	var aborted bool
	require.NoError(t, env.GetWorkflowResult(&aborted))
	assert.True(t, aborted, "the executor's error is recognised as an abort inside the workflow")

	assert.Equal(t, []string{"before:authz"}, ran,
		"the tool and the after-hook never ran")
}

// abortProxyWorkflow drives the executor the way the agent loop would and
// reports whether the hook's failure arrived as an abort.
func abortProxyWorkflow(ctx workflow.Context) (bool, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: testActivityTimeout,
	})

	executor := temporal_runtime.NewTemporalToolExecutor(ctx).WithToolCallHooks(
		[]agents.ToolCallHook{temporal_runtime.NewTemporalHookProxy(ctx, "authz")},
	)

	results := executor.ExecuteAll(context.Background(), []agents.ExecutableToolCall{{
		ToolName: "worker",
		Tool:     temporal_runtime.NewTemporalToolProxy(ctx, "Agent_worker", nil),
		ToolCall: hookCall(),
	}})

	return agents.IsToolCallAborted(results[0].Err), nil
}
