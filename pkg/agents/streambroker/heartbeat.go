package streambroker

import (
	"context"
	"log/slog"
	"time"
)

// startHeartbeat owns the Redis renewal timer for one execution.
// The stop function joins the emitter before stream closure applies replay retention.
func startHeartbeat(ctx context.Context, channel string, interval time.Duration, renew func(context.Context, string) error) func() {
	if channel == "" || interval <= 0 {
		return func() {}
	}

	// The execution context controls liveness; callers must not pass a browser subscription context.
	heartbeatCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	emit := func() {
		attemptCtx, stop := context.WithTimeout(heartbeatCtx, min(interval, 5*time.Second))
		defer stop()
		if err := renew(attemptCtx, channel); err != nil && heartbeatCtx.Err() == nil {
			slog.WarnContext(ctx, "agent stream heartbeat failed", "stream_id", channel, "error", err)
		}
	}

	// Renew immediately in the worker so a slow Redis request cannot block startup.
	go func() {
		defer close(done)
		emit()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		// Keep emitting during long model calls and tools, without adding replay events.
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				emit()
			}
		}
	}()

	// Join the emitter so it cannot renew the stream after the agent closes it.
	return func() {
		cancel()
		<-done
	}
}
