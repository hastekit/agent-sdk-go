package streambroker

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Renewal continues through quiet periods and transient failures, then joins on cancellation.
func TestHeartbeatLifecycle(t *testing.T) {
	var beats atomic.Int32
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stop := startHeartbeat(ctx, "quiet", 10*time.Millisecond, func(context.Context, string) error {
		if beats.Add(1) == 1 {
			return errors.New("transient failure")
		}
		return nil
	})
	defer stop()

	// Later attempts recover without requiring any response chunks.
	require.Eventually(t, func() bool { return beats.Load() >= 3 }, time.Second, time.Millisecond)

	// Repeated cleanup remains safe and no renewal survives its return.
	cancel()
	stop()
	count := beats.Load()
	require.Never(t, func() bool { return beats.Load() != count }, 30*time.Millisecond, time.Millisecond)
}

// A slow renewal cannot block startup or survive the returned cleanup function.
func TestHeartbeatJoinsInFlightRenewal(t *testing.T) {
	started, finished := make(chan struct{}), make(chan struct{})
	stop := startHeartbeat(t.Context(), "quiet", time.Minute, func(ctx context.Context, channel string) error {
		defer close(finished)
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	defer stop()

	// Wait for the request to be in flight before cancelling it.
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("renewal did not start")
	}
	stop()

	// Cleanup must wait for the in-flight request to exit.
	select {
	case <-finished:
	default:
		t.Fatal("cleanup returned before renewal finished")
	}
}

// Stopping one execution does not affect another execution's renewal loop.
func TestHeartbeatEmittersAreIndependent(t *testing.T) {
	var first, second atomic.Int32
	stopFirst := startHeartbeat(t.Context(), "first", 10*time.Millisecond, func(context.Context, string) error {
		first.Add(1)
		return nil
	})
	defer stopFirst()
	stopSecond := startHeartbeat(t.Context(), "second", 10*time.Millisecond, func(context.Context, string) error {
		second.Add(1)
		return nil
	})
	defer stopSecond()

	// Both timers advance independently while their executions are active.
	require.Eventually(t, func() bool { return first.Load() >= 2 && second.Load() >= 2 }, time.Second, time.Millisecond)
	stopFirst()
	firstCount, secondCount := first.Load(), second.Load()

	// Only the second execution continues after stopping the first.
	require.Eventually(t, func() bool { return second.Load() > secondCount }, time.Second, time.Millisecond)
	require.Equal(t, firstCount, first.Load())
}
