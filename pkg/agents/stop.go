package agents

import (
	"context"
	"errors"
	"time"
)

// Synthetic results for cancelled calls. Every function_call in history
// needs a matching output or the provider rejects later turns.
const (
	toolCancelledBeforeExec = "Tool call cancelled: the run was interrupted by the user before this tool executed."
	toolCancelledDuringExec = "Tool call cancelled: the run was stopped by the user while this tool was running."
)

// ErrToolCancelled reports a call the stop unwound. Executors turn it
// into ToolExecutionResult.Cancelled.
var ErrToolCancelled = errors.New("tool call cancelled: the run was stopped")

// DefaultCancelGrace bounds how long a cancelled call may take to unwind.
// Long enough that a clean unwind is never cut off, short enough that a
// tool ignoring ctx doesn't leave the stop looking broken.
const DefaultCancelGrace = 2 * time.Second

// StopWatcherFrom returns the broker's stop-watch capability, or nil if
// it has none.
func StopWatcherFrom(broker StreamBroker) StopWatcher {
	watcher, ok := broker.(StopWatcher)
	if !ok {
		return nil
	}
	return watcher
}

// RunStoppableTool runs one tool call so that stopping the run stops the
// work: it cancels the call's context when the stop flag lands, and
// abandons a tool that ignores it after grace, returning ErrToolCancelled.
//
// Every runtime calls this from wherever its tools actually run — the
// local executor, a Temporal activity, a Restate run step — because
// that's the only place holding a context the tool can honour. Cancelling
// a durable step from the workflow side ends the waiting, not the work:
// the step keeps running and a remote MCP server never hears about it.
//
// An abandoned tool runs on to its own end, unobserved. Callers crossing
// a durable boundary must translate ErrToolCancelled into a
// non-retryable (Temporal) or terminal (Restate) failure, or the runtime
// re-runs the call the user just cancelled.
//
// With no watcher or no stream, this is just fn.
func RunStoppableTool(
	ctx context.Context,
	watcher StopWatcher,
	grace time.Duration,
	params *ToolCall,
	fn func(ctx context.Context, params *ToolCall) (*ToolCallResponse, error),
) (*ToolCallResponse, error) {
	streamID := ""
	if params != nil {
		streamID = params.StreamID
	}

	if watcher == nil || streamID == "" {
		return fn(ctx, params)
	}

	stop, release := watcher.WatchStop(ctx, streamID)
	defer release()

	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Buffered so an abandoned call still reports and exits, into a result
	// nobody reads, instead of blocking forever on the send.
	type outcome struct {
		response *ToolCallResponse
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		response, err := fn(callCtx, params)
		done <- outcome{response, err}
	}()

	if grace <= 0 {
		grace = DefaultCancelGrace
	}

	select {
	case result := <-done:
		return result.response, result.err

	case <-stop:
		cancel()

		timer := time.NewTimer(grace)
		defer timer.Stop()

		select {
		case result := <-done:
			// Finished inside the window. A call that completed before the
			// cancellation landed keeps its real result — the work is done,
			// and discarding it would lose history the model should see.
			if result.err == nil || !errors.Is(result.err, context.Canceled) {
				return result.response, result.err
			}
			return nil, ErrToolCancelled
		case <-timer.C:
			return nil, ErrToolCancelled
		}

	case <-ctx.Done():
		// The caller's context went away — not a user stop.
		return nil, ctx.Err()
	}
}
