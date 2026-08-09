package temporal_runtime

import (
	"context"
	"errors"

	"github.com/hastekit/hastekit-sdk-go/pkg/agents"
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
	if err == nil || !errors.Is(err, agents.ErrToolCancelled) {
		return err
	}
	return temporal.NewNonRetryableApplicationError(err.Error(), ToolCancelledErrorType, nil)
}

// wasCancelled reports whether an activity failed because the run was
// stopped. ActivityError wraps the cause, so errors.As reaches it.
func wasCancelled(err error) bool {
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
}

func NewTemporalToolExecutor(workflowCtx workflow.Context) *TemporalToolExecutor {
	return &TemporalToolExecutor{workflowCtx: workflowCtx}
}

func (e *TemporalToolExecutor) ExecuteAll(ctx context.Context, executions []agents.ExecutableToolCall) []agents.ToolExecutionResult {
	results := make([]agents.ToolExecutionResult, len(executions))

	stopped := false
	for i, exec := range executions {
		if stopped {
			results[i] = agents.ToolExecutionResult{Err: agents.ErrToolCancelled, Cancelled: true}
			continue
		}

		resp, err := exec.Tool.Execute(ctx, exec.ToolCall)
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
