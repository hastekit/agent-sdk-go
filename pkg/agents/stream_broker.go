package agents

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// The built-in brokers implement the optional run-claim capability used
// for deterministic per-thread stream ids, and the optional stop-watch
// capability used to cancel tool calls that are already in flight.
var (
	_ RunClaimBroker = (*streambroker.MemoryStreamBroker)(nil)
	_ RunClaimBroker = (*streambroker.RedisStreamBroker)(nil)

	_ StopWatcher = (*streambroker.MemoryStreamBroker)(nil)
	_ StopWatcher = (*streambroker.RedisStreamBroker)(nil)

	_ RunFeed = (*streambroker.MemoryStreamBroker)(nil)
	_ RunFeed = (*streambroker.RedisStreamBroker)(nil)
)

// StreamIDForThread returns the broker channel a thread's runs stream on.
//
// It is deterministic, so a client that reconnects — or one that never
// held the id — can derive the same channel and rejoin the run in flight.
// The namespace is folded in so the same thread id in two namespaces
// never collides.
//
// A run with no thread to key on gets a random id: nothing could rejoin
// it anyway, and sharing one channel between unrelated runs would mix
// their transcripts.
func StreamIDForThread(namespace, threadID string) string {
	if threadID == "" {
		return uuid.NewString()
	}
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("hastekit:stream:"+namespace+"\x00"+threadID)).String()
}

// StreamIDForTask returns the broker channel a background task streams its
// progress on.
//
// A task gets a channel of its own rather than sharing the thread's. The
// thread's channel belongs to whatever run holds it, and a run claiming it
// resets the transcript — so a task publishing there would have its progress
// wiped by the next turn, and what survived would be interleaved into another
// run's stream keyed to a call that run never made.
//
// Like StreamIDForThread it is deterministic, so a client holding the task id
// can derive the channel rather than being told it. Task ids are expected to
// be unique per task: two tasks sharing one id share one channel.
func StreamIDForTask(namespace, threadID, taskID string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL,
		[]byte("hastekit:task:"+namespace+"\x00"+threadID+"\x00"+taskID)).String()
}

// StreamBroker provides an abstraction for streaming response chunks
// between activities/workers and clients. This enables streaming support
// for both Restate and Temporal runtimes.
type StreamBroker interface {
	// Publish sends a response chunk to subscribers of the given channel.
	// The channel is typically the run ID or a unique identifier for the execution.
	Publish(ctx context.Context, channel string, chunk *responses.ResponseChunk) error

	// Subscribe returns a channel that receives response chunks for the given channel.
	// The returned channel will be closed when Close is called or the context is cancelled.
	Subscribe(ctx context.Context, channel string) (<-chan *responses.ResponseChunk, error)

	// Close signals that no more chunks will be published to the channel.
	// This should close all subscriber channels for the given channel.
	Close(ctx context.Context, channel string) error

	// Stop records a stop request for the given channel. The agent loop
	// reads this via IsStopped at iteration boundaries and transitions
	// to completed when set. Idempotent.
	Stop(ctx context.Context, channel string) error

	// IsStopped reports whether Stop has been called for the channel.
	IsStopped(ctx context.Context, channel string) (bool, error)

	// EnqueueMessage pushes an input message onto the channel's queue.
	// The agent loop drains this queue at iteration boundaries — same
	// cadence as IsStopped — and folds queued messages into the current
	// run. Generic so future callers can deliver user messages, tool
	// outputs, etc., without a new transport.
	EnqueueMessage(ctx context.Context, channel string, msg history.Message) error

	// DrainMessages atomically returns and clears all queued messages
	// for the channel. Empty slice if nothing queued.
	DrainMessages(ctx context.Context, channel string) ([]history.Message, error)

	// IsActive reports whether the channel has an in-flight run — used
	// by the gateway to decide between enqueueing onto an existing
	// stream and starting a fresh one. A channel is active once
	// Subscribe has been called and stays active until Close.
	IsActive(ctx context.Context, channel string) (bool, error)
}

// StopWatcher is an optional StreamBroker capability that turns the
// poll-based stop flag into something to wait on. Tool wrappers use it to
// cancel a call mid-flight, so a stop lands during a long-running tool
// instead of only at the loop's next iteration boundary.
//
// Remote-backed brokers implement it by polling their own flag;
// in-process brokers close the channel directly from Stop. The
// durable-runtime proxies deliberately do not implement it — a poll
// goroutine is not something a workflow can journal — so under those
// runtimes the watch runs in the activity or run step instead.
type StopWatcher interface {
	// WatchStop returns a channel closed once Stop has been called for the
	// channel (immediately, if it already has), plus a release func the
	// caller must invoke.
	//
	// Implementations must not close it for any other reason, nor before
	// the stop is durably recorded: a subsequent IsStopped read has to
	// agree, since that read is what ends the run.
	WatchStop(ctx context.Context, channel string) (<-chan struct{}, func())
}

// RunClaimBroker is an optional StreamBroker capability that enables
// deterministic, per-thread stream IDs. With a deterministic streamID the
// same broker channel is reused across a thread's turns, so EnqueueOrStart
// must atomically decide, in one shot, whether a turn joins an in-flight
// run or starts a fresh one — and reset the channel when it starts, so a
// reused channel never replays a previous turn's transcript.
type RunClaimBroker interface {
	// EnqueueOrStart atomically routes a turn for streamID:
	//   - if a run is already live on the channel, it appends msgs to the
	//     run's queue and returns started=false;
	//   - otherwise it claims the channel, resets any stale transcript /
	//     queue / stop state, and returns started=true — the caller then
	//     Subscribes and runs with msgs as the run's input.
	// The claim is released by Close.
	EnqueueOrStart(ctx context.Context, streamID string, msgs []history.Message) (started bool, err error)
}

// Run lifecycle events, in the vocabulary the AG-UI stream uses.
const (
	RunEventStarted  = "RUN_STARTED"
	RunEventFinished = "RUN_FINISHED"
)

// RunEvent is one run beginning or ending, as a watcher of a whole namespace
// sees it.
type RunEvent = streambroker.RunEvent

// RunFeed is an optional StreamBroker capability: run lifecycle fanned in per
// namespace, so a client can watch every conversation in a namespace instead
// of one thread at a time.
//
// The thread streams cannot answer this. Each is keyed by its own channel, and
// a conversation that has not started yet has no channel to name — so a client
// sitting in one conversation could never learn that another had begun. This
// is a second, much thinner wire alongside them, carrying only the fact that a
// run started or ended.
//
// It is a feed rather than a broadcast because the interesting runs are the
// ones nobody was watching: a background task finishing at 3am. A client that
// reconnects passes back the cursor it last held and is told what it missed,
// which a fire-and-forget subscription could not do.
type RunFeed interface {
	// PublishRunEvent records one run beginning or ending. The namespace is
	// passed rather than derived: a channel id is a one-way hash of it.
	PublishRunEvent(ctx context.Context, event RunEvent) error

	// ReadRunEvents returns what happened in these namespaces after cursor,
	// waiting up to wait for something if there is nothing yet.
	//
	// The returned cursor is opaque and belongs to the implementation; a
	// client stores it and passes it back. An empty cursor means "from now",
	// not "from the beginning" — a client attaching for the first time wants
	// what happens next, not a replay of the day.
	ReadRunEvents(ctx context.Context, namespaces []string, cursor string, wait time.Duration) ([]RunEvent, string, error)
}

// publishRunEvent tells whoever is watching the namespace that a run began or
// ended.
//
// This is a second, much thinner wire than the run's own stream, and it exists
// because that stream cannot answer the question: it is keyed by the thread's
// channel, so a browser sitting in one conversation has nothing to attach to
// that would tell it another had started — least of all a conversation that
// did not exist when it attached.
//
// Best effort. A run is not worth failing over a notification nobody may be
// listening for, and the runs themselves are in the thread list regardless.
func (e *Agent) publishRunEvent(ctx context.Context, event string, in *AgentInput, runID string) {
	feed, ok := e.streamBroker.(RunFeed)
	if !ok || in == nil {
		return
	}

	if err := feed.PublishRunEvent(ctx, RunEvent{
		Event:     event,
		Namespace: in.Namespace,
		ThreadID:  in.ThreadID,
		RunID:     runID,
		AgentName: e.Name,
		StreamID:  in.StreamID,
	}); err != nil {
		slog.WarnContext(ctx, "failed to publish a run event",
			slog.String("event", event), slog.String("thread_id", in.ThreadID), slog.Any("error", err))
	}
}
