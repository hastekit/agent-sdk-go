package agui

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
)

// How a long poll decides how long to hold a connection open, shared by the
// run feed and by a rejoin asked to wait for the run it was told about.
const (
	// defaultWatchWait is how long a poll holds the connection when the
	// client does not say. Short enough to sit inside the timeouts proxies
	// usually impose, long enough that an idle thread is nearly free.
	defaultWatchWait = 25 * time.Second

	// maxWatchWait bounds what a client can ask for, so one cannot pin a
	// connection open indefinitely.
	maxWatchWait = 5 * time.Minute

	// watchPoll is how often a wait re-reads the broker. Whether a run has
	// begun is not something the broker can be subscribed to — IsActive is a
	// question, not a signal — so a wait asks repeatedly.
	watchPoll = 250 * time.Millisecond
)

// watchWait reads the requested wait, in Go duration form ("25s") or bare
// seconds ("25"), clamped to something a server is willing to hold open.
//
// fallback is what an unset or unreadable `wait` means. It differs by
// endpoint: a watch is a long poll and waits by default, while a rejoin
// answers at once unless asked to hold on, which is what it has always done
// and what a client opening an idle thread expects.
func watchWait(r *http.Request, fallback time.Duration) time.Duration {
	raw := r.URL.Query().Get("wait")
	if raw == "" {
		return fallback
	}

	wait, err := time.ParseDuration(raw)
	if err != nil {
		seconds, secErr := strconv.Atoi(raw)
		if secErr != nil {
			return fallback
		}
		wait = time.Duration(seconds) * time.Second
	}

	if wait < 0 {
		return 0
	}
	return min(wait, maxWatchWait)
}

// waitForRunChange blocks until the thread's state differs from was, the wait
// runs out, or the client gives up. It returns the state as it last read it.
func waitForRunChange(
	ctx context.Context,
	broker agents.StreamBroker,
	streamID string,
	was bool,
	wait time.Duration,
) (bool, error) {
	deadline := time.Now().Add(wait)

	for {
		active, err := broker.IsActive(ctx, streamID)
		if err != nil {
			return was, err
		}
		if active != was {
			return active, nil
		}
		if !time.Now().Before(deadline) {
			return active, nil
		}

		select {
		case <-ctx.Done():
			return active, ctx.Err()
		case <-time.After(watchPoll):
		}
	}
}
