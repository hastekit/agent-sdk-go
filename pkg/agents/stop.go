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

	// The assistant turn a stopped run ends on — what the transcript shows
	// in place of the answer the user cut short.
	runCancelledNotice = "Cancelled by user"
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
// work. It is RunStoppable keyed on the call's own stream.
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

	return RunStoppable(ctx, watcher, grace, streamID, func(callCtx context.Context) (*ToolCallResponse, error) {
		return fn(callCtx, params)
	})
}

// RunStoppable runs fn so that stopping the run stops the work: it cancels
// fn's context when the stop flag lands on streamID, and abandons an fn
// that ignores it after grace, returning ErrToolCancelled.
//
// Every runtime calls this from wherever its tools actually run — the
// local executor, a Temporal activity, a Restate run step — because
// that's the only place holding a context the tool can honour. Cancelling
// a durable step from the workflow side ends the waiting, not the work:
// the step keeps running and a remote MCP server never hears about it.
//
// An abandoned call runs on to its own end, unobserved. Callers crossing
// a durable boundary must translate ErrToolCancelled into a
// non-retryable (Temporal) or terminal (Restate) failure, or the runtime
// re-runs the call the user just cancelled.
//
// It is generic because the shape of a tool's result differs by call
// site: the loop's executors hand back a *ToolCallResponse, while a
// Temporal activity interceptor only sees `any`. Both need the same
// cancellation semantics, and a caller that adapted this by assigning
// into a captured variable would race the abandoned goroutine.
//
// With no watcher or no stream, this is just fn.
func RunStoppable[T any](
	ctx context.Context,
	watcher StopWatcher,
	grace time.Duration,
	streamID string,
	fn func(ctx context.Context) (T, error),
) (T, error) {
	if watcher == nil || streamID == "" {
		return fn(ctx)
	}

	stop, release := watcher.WatchStop(ctx, streamID)
	defer release()

	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Buffered so an abandoned call still reports and exits, into a result
	// nobody reads, instead of blocking forever on the send.
	type outcome struct {
		value T
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		value, err := fn(callCtx)
		done <- outcome{value, err}
	}()

	if grace <= 0 {
		grace = DefaultCancelGrace
	}

	var zero T

	select {
	case result := <-done:
		return result.value, result.err

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
				return result.value, result.err
			}
			return zero, ErrToolCancelled
		case <-timer.C:
			return zero, ErrToolCancelled
		}

	case <-ctx.Done():
		// The caller's context went away — not a user stop.
		return zero, ctx.Err()
	}
}
