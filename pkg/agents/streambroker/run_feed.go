package streambroker

import (
	"context"
	"sync"
	"time"
)

// RunEvent is one run beginning or ending, as a watcher of a whole namespace
// sees it. agents.RunEvent aliases it: the type belongs with the brokers that
// carry it, and the agents package imports this one rather than the reverse.
type RunEvent struct {
	Event     string    `json:"event"`
	Namespace string    `json:"namespace"`
	ThreadID  string    `json:"threadId"`
	RunID     string    `json:"runId,omitempty"`
	AgentName string    `json:"agentName,omitempty"`
	StreamID  string    `json:"streamId"`
	At        time.Time `json:"at"`
}

// Run lifecycle events, in the vocabulary the AG-UI stream uses.
const (
	RunEventStarted  = "RUN_STARTED"
	RunEventFinished = "RUN_FINISHED"
)

// maxFeedPerNamespace caps how much of a namespace's run history a process
// keeps. It is a reconnect window, not an archive: a client that falls further
// behind than this reconciles from the thread list, which is where the runs
// actually are.
const maxFeedPerNamespace = 512

// feedHub holds the run events a process knows about and wakes whoever is
// waiting for them.
//
// Both brokers use it, and the only difference is where events come from: the
// in-process broker appends its own, while the Redis broker appends what
// arrives on a pub/sub subscription it holds once for the whole process. That
// is the point of the split — a server with a hundred browsers watching holds
// one Redis subscription, not a hundred blocked reads.
//
// Waiting readers are woken rather than handed the event: a wake makes every
// reader re-read under the lock, so one append serves all of them and none can
// slip past a reader that was between checks.
type feedHub struct {
	mu      sync.Mutex
	events  map[string][]RunEvent
	waiters []chan struct{}
}

func newFeedHub() *feedHub {
	return &feedHub{events: map[string][]RunEvent{}}
}

// append records an event and wakes every reader.
func (h *feedHub) append(event RunEvent) {
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	events := append(h.events[event.Namespace], event)
	if len(events) > maxFeedPerNamespace {
		events = events[len(events)-maxFeedPerNamespace:]
	}
	h.events[event.Namespace] = events

	for _, waiter := range h.waiters {
		close(waiter)
	}
	h.waiters = nil
}

// read returns what happened in these namespaces after cursor, waiting up to
// wait if there is nothing yet.
//
// The cursor is the timestamp of the last event a reader saw. A timestamp
// rather than a position because the position is this process's own: behind a
// load balancer the next poll may land on another replica, which received the
// same events over pub/sub but not necessarily in the same order or from the
// same starting point. The timestamp is the publisher's, so it means the same
// thing everywhere.
//
// An empty cursor means "from now". A client attaching for the first time
// wants what happens next, not a replay of the day.
func (h *feedHub) read(
	ctx context.Context,
	namespaces []string,
	cursor time.Time,
	wait time.Duration,
) ([]RunEvent, time.Time) {
	if cursor.IsZero() {
		cursor = time.Now().UTC()
	}
	deadline := time.Now().Add(wait)

	for {
		events, waiter := h.collect(namespaces, cursor)
		if len(events) > 0 {
			return events, events[len(events)-1].At
		}
		if !time.Now().Before(deadline) {
			return nil, cursor
		}

		select {
		case <-ctx.Done():
			return nil, cursor
		case <-waiter:
			// Something landed; go round and collect it.
		case <-time.After(time.Until(deadline)):
			events, _ := h.collect(namespaces, cursor)
			if len(events) > 0 {
				return events, events[len(events)-1].At
			}
			return nil, cursor
		}
	}
}

// collect gathers everything after cursor, and returns a channel that closes
// on the next append when there was nothing.
//
// The waiter is registered under the same lock the read happens in, which is
// what keeps an event appended between the two from being missed.
func (h *feedHub) collect(namespaces []string, cursor time.Time) ([]RunEvent, chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()

	var out []RunEvent
	for _, namespace := range namespaces {
		for _, event := range h.events[namespace] {
			if event.At.After(cursor) {
				out = append(out, event)
			}
		}
	}

	if len(out) > 0 {
		return out, nil
	}

	waiter := make(chan struct{})
	h.waiters = append(h.waiters, waiter)
	return nil, waiter
}

// ParseFeedCursor reads the cursor a client hands back. An unreadable one is
// treated as absent, which means "from now" — better than replaying a window
// the client may already have shown.
func ParseFeedCursor(cursor string) time.Time {
	if cursor == "" {
		return time.Time{}
	}
	at, err := time.Parse(time.RFC3339Nano, cursor)
	if err != nil {
		return time.Time{}
	}
	return at.UTC()
}

// FormatFeedCursor encodes a cursor for a client to hand back.
func FormatFeedCursor(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339Nano)
}

// --- in-process broker ------------------------------------------------------

// PublishRunEvent implements the RunFeed capability.
func (b *MemoryStreamBroker) PublishRunEvent(_ context.Context, event RunEvent) error {
	b.runFeed.append(event)
	return nil
}

// ReadRunEvents implements the RunFeed capability.
func (b *MemoryStreamBroker) ReadRunEvents(
	ctx context.Context,
	namespaces []string,
	cursor string,
	wait time.Duration,
) ([]RunEvent, string, error) {
	events, next := b.runFeed.read(ctx, namespaces, ParseFeedCursor(cursor), wait)
	return events, FormatFeedCursor(next), nil
}
