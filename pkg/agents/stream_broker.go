package agents

import (
	"context"

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
