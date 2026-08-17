package temporal_runtime

import (
	"context"
	"errors"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// ToolCancelledErrorType marks the activity failure a stopped tool call
// reports, so the workflow can tell it from a tool that failed on its own.
const ToolCancelledErrorType = "ToolCancelled"

// cancellationError converts a stopped tool call into an activity failure
// Temporal will not retry. Nothing here sets a RetryPolicy, so the server
// default of unlimited attempts applies: a retryable error would re-run
// the tool the user just stopped. Other errors pass through untouched.
func cancellationError(err error) error {
	if err == nil || !stoppedWork(err) {
		return err
	}
	return temporal.NewNonRetryableApplicationError(err.Error(), ToolCancelledErrorType, nil)
}

// stoppedWork reports whether an error is work the stop unwound — a tool call
// or a streaming model call. Both must reach the runtime as a failure it will
// not retry.
func stoppedWork(err error) bool {
	return errors.Is(err, agents.ErrToolCancelled) || errors.Is(err, agents.ErrModelCallStopped)
}

// wasCancelled reports whether an activity failed because the run was
// stopped. ActivityError wraps the cause, so errors.As reaches it.
func wasCancelled(err error) bool {
	return WasStopped(err)
}

// WasStopped reports whether an activity failed because the run was stopped,
// rather than because the work itself failed. It is exported for workers that
// register activities of their own — a no-code host builds its agents from
// configuration and cannot use this package's, but still has to tell a stop
// from a failure on the way back.
func WasStopped(err error) bool {
	var appErr *temporal.ApplicationError
	return errors.As(err, &appErr) && appErr.Type() == ToolCancelledErrorType
}

// TemporalToolExecutor executes tool calls as Temporal activities.
//
// Stopping is handled inside the activity, where the tool actually runs
// (see TemporalTool.Execute); cancelling from here would end the
// workflow's wait and leave the work running. This side only reads the
// outcome, and stops the round on a cancelled call since the rest would
// only start and cancel themselves. That comes from a journaled activity
// result, so a replay decides the same way.
type TemporalToolExecutor struct {
	workflowCtx workflow.Context

	// hooks run here, in the workflow, so each of their methods is its own
	// activity — see TemporalToolCallHookProxy.
	hooks []agents.ToolCallHook
}

func NewTemporalToolExecutor(workflowCtx workflow.Context) *TemporalToolExecutor {
	return &TemporalToolExecutor{workflowCtx: workflowCtx}
}

var _ agents.HookAwareToolExecutor = (*TemporalToolExecutor)(nil)

// WithToolCallHooks implements agents.HookAwareToolExecutor.
func (e *TemporalToolExecutor) WithToolCallHooks(hooks []agents.ToolCallHook) agents.ToolExecutor {
	bound := *e
	bound.hooks = hooks
	return &bound
}

func (e *TemporalToolExecutor) ExecuteAll(ctx context.Context, executions []agents.ExecutableToolCall) []agents.ToolExecutionResult {
	results := make([]agents.ToolExecutionResult, len(executions))

	stopped := false
	for i, exec := range executions {
		if stopped {
			results[i] = agents.ToolExecutionResult{Err: agents.ErrToolCancelled, Cancelled: true}
			continue
		}

		resp, err := agents.RunWithToolCallHooks(ctx, e.hooks, exec.ToolCall, exec.Tool.Execute)
		results[i] = agents.ToolExecutionResult{
			Response:  resp,
			Err:       err,
			Cancelled: wasCancelled(err),
		}
		stopped = results[i].Cancelled
	}

	// TODO: Parallelize temporal tool execution.

	return results
}
