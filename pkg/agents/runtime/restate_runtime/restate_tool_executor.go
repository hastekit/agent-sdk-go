package restate_runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	restate "github.com/restatedev/sdk-go"
)

// ToolCancelledErrorCode marks the run-step failure a stopped tool call
// reports. Restate errors cross the journal as a code and message, not a
// Go type, so the code is what the handler matches on; 499 is the
// conventional "client closed request". Untyped because restate's Code
// type is in an internal package.
const ToolCancelledErrorCode = 499

// cancellationError converts a stopped tool call into a terminal run-step
// failure. Restate retries the whole invocation on a non-terminal error,
// replaying into this step and re-running the tool the user just stopped.
// Other errors pass through untouched.
func cancellationError(err error) error {
	if agents.IsToolCallAborted(err) {
		return abortError(err)
	}
	if err == nil || !stoppedWork(err) {
		return err
	}
	return restate.TerminalError(err, ToolCancelledErrorCode)
}

// stoppedWork reports whether an error is work the stop unwound — a tool call
// or a streaming model call. Both must reach Restate as a terminal failure it
// will not replay.
func stoppedWork(err error) bool {
	return errors.Is(err, agents.ErrToolCancelled) || errors.Is(err, agents.ErrModelCallStopped)
}

// wasCancelled reports whether a run step failed because of a stop.
func wasCancelled(err error) bool {
	return err != nil && restate.ErrorCode(err) == ToolCancelledErrorCode
}

// RestateToolExecutor executes tool calls as Restate run steps.
//
// Stopping is handled inside the step, where the tool actually runs (see
// RestateTool.Execute); abandoning the step from here would end the
// handler's wait and leave the work running. This side only reads the
// outcome, and stops the round on a cancelled call since the rest would
// only start and cancel themselves. That comes from a journaled step
// result, so a replay decides the same way.
//
// It holds no middlewares: the middlewares' wraps run inside the tool's step, and what
// comes back is already what the chain made of the call.
type RestateToolExecutor struct {
	restateCtx restate.WorkflowContext
}

func NewRestateToolExecutor(restateCtx restate.WorkflowContext) *RestateToolExecutor {
	return &RestateToolExecutor{restateCtx: restateCtx}
}

func (e *RestateToolExecutor) ExecuteAll(ctx context.Context, executions []agents.ExecutableToolCall) []agents.ToolExecutionResult {
	results := make([]agents.ToolExecutionResult, len(executions))

	stopped := false
	for i, exec := range executions {
		if stopped {
			results[i] = agents.ToolExecutionResult{Err: agents.ErrToolCancelled, Cancelled: true}
			continue
		}

		resp, err := agents.ExecuteToolWithMiddleware(ctx, nil, exec, exec.Tool.Execute)
		if wasAborted(err) {
			err = fmt.Errorf("%w: %w", agents.ErrToolCallAborted, err)
		}
		results[i] = agents.ToolExecutionResult{
			Response:  resp,
			Err:       err,
			Cancelled: wasCancelled(err),
		}
		stopped = results[i].Cancelled
	}

	// TODO: parallelize restate tool execution.

	return results
}

// wasAborted reports whether a run step failed because a middleware ended the run.
func wasAborted(err error) bool {
	return err != nil && restate.ErrorCode(err) == ToolCallAbortedErrorCode
}
