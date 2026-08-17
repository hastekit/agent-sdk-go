package streambroker

import (
	"context"
	"sync"

	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// MemoryStreamBroker is an in-memory implementation of StreamBroker.
// It's suitable for testing and local development, or when all components
// run in the same process.
//
// Note: This broker does not persist across restarts. For production
// deployments with separate processes, use RedisStreamBroker.
type MemoryStreamBroker struct {
	mu          sync.RWMutex
	subscribers map[string][]chan *responses.ResponseChunk
	closed      map[string]bool
	stopped     map[string]bool
	stopChans   map[string]chan struct{}
	queues      map[string][]messages.Message
	live        map[string]bool

	// transcripts retains what was published per channel so a subscriber
	// arriving mid-run — or after it finished — sees the whole run rather
	// than the tail. RedisStreamBroker is rejoinable for the same reason;
	// this keeps the in-process broker behaving the same way.
	transcripts map[string][]*responses.ResponseChunk
}

// maxTranscript caps retained chunks per channel, mirroring the Redis
// broker's MAXLEN: a rejoiner wants the run, not unbounded history.
const maxTranscript = 2000

// NewMemoryStreamBroker creates a new in-memory stream broker.
func NewMemoryStreamBroker() *MemoryStreamBroker {
	return &MemoryStreamBroker{
		subscribers: make(map[string][]chan *responses.ResponseChunk),
		closed:      make(map[string]bool),
		stopped:     make(map[string]bool),
		stopChans:   make(map[string]chan struct{}),
		queues:      make(map[string][]messages.Message),
		live:        make(map[string]bool),
		transcripts: make(map[string][]*responses.ResponseChunk),
	}
}

// Publish sends a response chunk to all subscribers of the given channel.
func (b *MemoryStreamBroker) Publish(ctx context.Context, channel string, chunk *responses.ResponseChunk) error {
	b.mu.Lock()

	// Don't publish to closed channels
	if b.closed[channel] {
		b.mu.Unlock()
		return nil
	}

	transcript := b.transcripts[channel]
	if len(transcript) >= maxTranscript {
		transcript = transcript[1:]
	}
	b.transcripts[channel] = append(transcript, chunk)

	subscribers := append([]chan *responses.ResponseChunk(nil), b.subscribers[channel]...)
	b.mu.Unlock()

	for _, sub := range subscribers {
		select {
		case sub <- chunk:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return nil
}

// Subscribe returns a channel that receives response chunks for the given channel.
// The buffer size is 100 chunks to handle bursts.
func (b *MemoryStreamBroker) Subscribe(ctx context.Context, channel string) (<-chan *responses.ResponseChunk, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	transcript := b.transcripts[channel]

	// A finished run still replays: a client that reconnects after it ended
	// gets the transcript and then the close, rather than an empty stream.
	if b.closed[channel] {
		ch := make(chan *responses.ResponseChunk, len(transcript))
		for _, chunk := range transcript {
			ch <- chunk
		}
		close(ch)
		return ch, nil
	}

	// Buffered past the replay so publishing doesn't block immediately.
	ch := make(chan *responses.ResponseChunk, len(transcript)+100)
	for _, chunk := range transcript {
		ch <- chunk
	}
	b.subscribers[channel] = append(b.subscribers[channel], ch)

	// Handle context cancellation
	go func() {
		<-ctx.Done()
		b.unsubscribe(channel, ch)
	}()

	return ch, nil
}

// unsubscribe removes a subscriber from a channel.
func (b *MemoryStreamBroker) unsubscribe(channel string, ch chan *responses.ResponseChunk) {
	b.mu.Lock()
	defer b.mu.Unlock()

	subscribers := b.subscribers[channel]
	for i, sub := range subscribers {
		if sub == ch {
			// Remove subscriber
			b.subscribers[channel] = append(subscribers[:i], subscribers[i+1:]...)
			close(ch)
			break
		}
	}
}

// Close signals that no more chunks will be published to the channel.
// This closes all subscriber channels for the given channel.
func (b *MemoryStreamBroker) Close(ctx context.Context, channel string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Mark channel as closed and release the run claim so the next turn
	// for this channel starts a fresh run.
	b.closed[channel] = true
	delete(b.live, channel)

	// Close all subscriber channels
	for _, ch := range b.subscribers[channel] {
		close(ch)
	}

	// Clear subscribers
	delete(b.subscribers, channel)

	return nil
}

// EnqueueOrStart implements RunClaimBroker: atomically join an in-flight
// run on channel, or claim + reset the channel for a fresh run.
func (b *MemoryStreamBroker) EnqueueOrStart(ctx context.Context, channel string, msgs []messages.Message) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.live[channel] {
		b.queues[channel] = append(b.queues[channel], msgs...)
		return false, nil
	}

	// Win the claim and clear stale state so the fresh run's Subscribe /
	// Publish aren't short-circuited by a prior run on the same channel.
	b.live[channel] = true
	delete(b.queues, channel)
	delete(b.closed, channel)
	delete(b.stopped, channel)
	delete(b.transcripts, channel)
	// Drop the previous run's closed stop signal.
	delete(b.stopChans, channel)
	return true, nil
}

// Stop records a stop request for the given channel and closes its stop
// signal so watchers (see WatchStop) unblock immediately. Idempotent.
func (b *MemoryStreamBroker) Stop(ctx context.Context, channel string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped[channel] {
		return nil
	}
	// Before flipping the flag: stopSignal hands back an already-closed
	// channel once it is set.
	ch := b.stopSignal(channel)
	b.stopped[channel] = true
	close(ch)
	return nil
}

// WatchStop implements StopWatcher. Nothing to poll in-process: Stop
// closes the channel directly. The release func is a no-op.
func (b *MemoryStreamBroker) WatchStop(ctx context.Context, channel string) (<-chan struct{}, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stopSignal(channel), func() {}
}

// stopSignal returns the channel Stop closes, creating it on first use.
// One created after the stop was recorded starts out closed, so WatchStop
// and IsStopped never disagree. Callers must hold b.mu.
func (b *MemoryStreamBroker) stopSignal(channel string) chan struct{} {
	ch, ok := b.stopChans[channel]
	if !ok {
		ch = make(chan struct{})
		if b.stopped[channel] {
			close(ch)
		}
		b.stopChans[channel] = ch
	}
	return ch
}

// IsStopped reports whether Stop has been called for the channel.
func (b *MemoryStreamBroker) IsStopped(ctx context.Context, channel string) (bool, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.stopped[channel], nil
}

// EnqueueMessage appends a message onto the channel's pending queue.
// The message remains available until DrainMessages is called.
func (b *MemoryStreamBroker) EnqueueMessage(ctx context.Context, channel string, msg messages.Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.queues[channel] = append(b.queues[channel], msg)
	return nil
}

// DrainMessages atomically returns and removes all queued messages
// for the channel.
func (b *MemoryStreamBroker) DrainMessages(ctx context.Context, channel string) ([]messages.Message, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	msgs := b.queues[channel]
	delete(b.queues, channel)
	return msgs, nil
}

// IsActive reports whether the channel has a run in flight. The run claim
// is the primary signal: a run whose only subscriber navigated away is
// still running, and is exactly what a client wants to rejoin.
func (b *MemoryStreamBroker) IsActive(ctx context.Context, channel string) (bool, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed[channel] {
		return false, nil
	}
	return b.live[channel] || len(b.subscribers[channel]) > 0, nil
}

// Reset clears all subscribers and closed state.
// Useful for testing.
func (b *MemoryStreamBroker) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Close all existing subscriber channels
	for _, subs := range b.subscribers {
		for _, ch := range subs {
			close(ch)
		}
	}

	b.subscribers = make(map[string][]chan *responses.ResponseChunk)
	b.closed = make(map[string]bool)
	b.stopped = make(map[string]bool)
	b.stopChans = make(map[string]chan struct{})
	b.queues = make(map[string][]messages.Message)
	b.live = make(map[string]bool)
	b.transcripts = make(map[string][]*responses.ResponseChunk)
}
