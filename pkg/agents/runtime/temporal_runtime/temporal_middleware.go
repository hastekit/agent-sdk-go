package temporal_runtime

import (
	"errors"

	"go.temporal.io/sdk/temporal"
)

// ToolCallAbortedErrorType marks the activity failure a middleware's error becomes,
// which ends the run.
const ToolCallAbortedErrorType = "ToolCallAborted"

// abortError converts a middleware's error into an activity failure Temporal will not
// retry. No RetryPolicy is set anywhere here, so the server default of unlimited
// attempts applies, and a retryable failure would keep re-running a wrap that
// has already said no — the run would hang on the refusal instead of ending on
// it.
//
// Only what the middleware itself returned passes through here. A failure Temporal
// raises around it — a timeout, a lost worker — is not the middleware's answer and
// stays retryable, which is what should happen to those.
func abortError(err error) error {
	if err == nil {
		return err
	}
	return temporal.NewNonRetryableApplicationError(err.Error(), ToolCallAbortedErrorType, nil)
}

// WasAborted reports whether an activity failed because a middleware ended the run,
// rather than because the work itself failed. The agent loop does not need it —
// a middleware error is recognised by where it came from, not by its type — but a
// worker that registers activities of its own has only the error to go on, the
// same way WasStopped serves one.
func WasAborted(err error) bool {
	var appErr *temporal.ApplicationError
	return errors.As(err, &appErr) && appErr.Type() == ToolCallAbortedErrorType
}
