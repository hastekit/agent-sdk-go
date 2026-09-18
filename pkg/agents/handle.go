package agents

import (
	"context"
	"errors"
	"fmt"

	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// AgentHandle observes one run. Wait is independent of event consumption.
// Chunks has one consumer and bounded buffering; an incomplete stream is
// reported by Wait as ErrStreamOverflow alongside the final result.
type AgentHandle struct {
	StreamID string
	Chunks   <-chan *responses.ResponseChunk

	broker    StreamBroker
	done      chan struct{}
	result    *AgentOutput
	err       error
	streamErr error
}

// Stop signals the agent to stop and transition to completed state.
//
// A tool call already running has its context cancelled, and is abandoned
// after a grace period if it ignores that — it keeps running in the
// background, unobserved. Each cancelled call gets a synthetic result so
// history keeps its call/result pairing. An in-flight model request
// has its context cancelled too, including while waiting for HTTP headers.
//
// Reaching a running tool needs a broker implementing StopWatcher (the
// memory and Redis brokers do); the tool wrapper watches that flag
// wherever the tool runs. With a plain broker the stop still lands, at
// the loop's next iteration boundary.
//
// Use Wait or Result to block until the run finishes.
func (h *AgentHandle) Stop(ctx context.Context) error {
	return h.broker.Stop(ctx, h.StreamID)
}

// EnqueueMessage pushes msg onto the run's broker queue. The agent
// drains the queue at iteration boundaries (alongside the IsStopped
// check) and folds queued messages into the current run via
// ProcessIncomingMessages — approval responses become approve/reject
// queues; other input messages slot into the next LLM call.
//
// Use this to deliver follow-ups (user messages, approval/rejection
// decisions) to a run that's still in flight. For runs that have
// already paused and exited, the next agent.Execute on the same
// thread is the right entry instead.
func (h *AgentHandle) EnqueueMessage(ctx context.Context, msg history.Message) error {
	return h.broker.EnqueueMessage(ctx, h.StreamID, msg)
}

// Wait waits for completion. An optional context cancels only this wait, never
// the execution. Omitting it waits indefinitely. It is safe to call repeatedly
// or concurrently, without consuming Chunks.
func (h *AgentHandle) Wait(contexts ...context.Context) (*AgentOutput, error) {
	ctx := context.Background()
	if len(contexts) > 1 {
		return nil, fmt.Errorf("Wait accepts at most one context")
	}
	if len(contexts) == 1 {
		ctx = contexts[0]
	}
	select {
	case <-h.done:
		return h.result, errors.Join(h.err, h.streamErr)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Result returns the final result. It is equivalent to Wait; event draining is
// automatic. Prefer Run when no events are needed.
func (h *AgentHandle) Result() (*AgentOutput, error) { return h.Wait() }

// Cancel requests cooperative cancellation, including remote work through the
// shared broker. Its context bounds the request, not the execution lifetime.
func (h *AgentHandle) Cancel(ctx context.Context) error { return h.Stop(ctx) }

// subscribe forwards events independently of the consumer, so waiting for the
// result never blocks execution. Cleanup drains events already delivered by the broker.
func (e *Agent) subscribe(ctx context.Context, streamID string) (*AgentHandle, func(), error) {
	subCtx, cancel := context.WithCancel(ctx)
	chunks, err := e.streamBroker.Subscribe(subCtx, streamID)
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("subscribe: %w", err)
	}
	out := make(chan *responses.ResponseChunk, StreamBufferSize)
	h := &AgentHandle{StreamID: streamID, broker: e.streamBroker, done: make(chan struct{}), Chunks: out}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		defer close(out)
		for chunk := range chunks {
			if h.streamErr != nil {
				continue
			}
			select {
			case out <- chunk:
			default:
				h.streamErr = ErrStreamOverflow
			}
		}
	}()
	return h, func() { cancel(); <-drained }, nil
}
