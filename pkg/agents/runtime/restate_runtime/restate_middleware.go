package restate_runtime

import restate "github.com/restatedev/sdk-go"

// ToolCallAbortedErrorCode marks the run-step failure a middleware's error becomes,
// which ends the run. Untyped for the same reason ToolCancelledErrorCode is:
// restate's Code type is in an internal package.
//
// Not 500, tempting as it is for "the run failed": restate.ErrorCode answers 500
// for any error carrying no code of its own, so 500 here would be
// indistinguishable from every other failure to anything reading the code back.
const ToolCallAbortedErrorCode = 598

// abortError converts a middleware's error into a terminal step failure. Restate
// retries the whole invocation on a non-terminal error, replaying into this step
// and re-running a wrap that has already said no — the run would hang on the
// refusal rather than ending on it.
//
// Only what the middleware itself returned passes through here, so a failure Restate
// raises around the step keeps its own retry behaviour.
func abortError(err error) error {
	if err == nil {
		return err
	}
	return restate.TerminalError(err, ToolCallAbortedErrorCode)
}
