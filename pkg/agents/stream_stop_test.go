package agents_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// textItemDone is the chunk the accumulator folds a finished message from.
func textItemDone(id, text string) *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfOutputItemDone: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{
			Item: responses.ChunkOutputItemData{
				Type: "message",
				Id:   id,
				Role: constants.RoleAssistant,
				Content: &responses.ChunkOutputItemContent{
					{OfOutputText: &responses.OutputTextContent{Text: text}},
				},
			},
		},
	}
}

// The stop cuts the read short rather than waiting for the provider to finish
// saying what it was saying.
func TestReadStream_StopsMidStream(t *testing.T) {
	stream := make(chan *responses.ResponseChunk)
	ctx, cancel := context.WithCancel(context.Background())

	seen := 0
	done := make(chan struct{})
	var resp *responses.Response
	var err error

	go func() {
		defer close(done)
		acc := agents.Accumulator{}
		resp, err = acc.ReadStream(ctx, stream, func(*responses.ResponseChunk) { seen++ })
	}()

	stream <- textItemDone("msg_1", "the model was saying")
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ReadStream did not give up when the context was cancelled")
	}

	assert.ErrorIs(t, err, agents.ErrModelCallStopped)
	assert.Nil(t, resp, "a stopped call has no answer")
	assert.Equal(t, 1, seen, "chunks that arrived before the stop still reached the client")

	// The provider is still writing into a channel nobody reads. Draining is
	// what keeps its sender from blocking forever.
	select {
	case stream <- textItemDone("msg_2", "and kept going"):
	case <-time.After(time.Second):
		t.Fatal("the unread remainder was not drained; the provider would block")
	}
	close(stream)
}

// Reading to the end is unaffected: the response is whatever the model
// finished saying.
func TestReadStream_CompletesNormally(t *testing.T) {
	stream := make(chan *responses.ResponseChunk, 2)
	stream <- textItemDone("msg_1", "all done")
	close(stream)

	acc := agents.Accumulator{}
	resp, err := acc.ReadStream(context.Background(), stream, func(*responses.ResponseChunk) {})

	require.NoError(t, err)
	require.Len(t, resp.Output, 1)
	assert.Equal(t, "all done", (*resp.Output[0].OfOutputMessage.Content)[0].OfOutputText.Text)
}

// The context is cancelled when the run is stopped, which is what reaches the
// provider's own request.
func TestStopCancelContext(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()

	ctx, cancel := agents.StopCancelContext(context.Background(), broker, "stream-1")
	defer cancel()

	require.NoError(t, ctx.Err(), "not cancelled before the stop")
	require.NoError(t, broker.Stop(context.Background(), "stream-1"))

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("the stop did not cancel the context")
	}
}

// With nothing to watch, the context passes through — so a call site can wrap
// unconditionally.
func TestStopCancelContext_NoWatcherOrStream(t *testing.T) {
	base := context.Background()

	ctx, cancel := agents.StopCancelContext(base, nil, "stream-1")
	cancel()
	assert.Equal(t, base, ctx)

	ctx, cancel = agents.StopCancelContext(base, streambroker.NewMemoryStreamBroker(), "")
	cancel()
	assert.Equal(t, base, ctx)
}

// streamingLLM answers by streaming, and blocks partway through until it is
// released or its context is cancelled — a model still talking when the user
// presses stop.
type streamingLLM struct {
	started  chan struct{}
	unblock  chan struct{}
	finished bool
}

func (l *streamingLLM) NewStreamingResponses(ctx context.Context, in *responses.Request, cb func(*responses.ResponseChunk)) (*responses.Response, error) {
	stream := make(chan *responses.ResponseChunk)

	go func() {
		defer close(stream)
		select {
		case stream <- textItemDone("msg_1", "half an answer"):
		case <-ctx.Done():
			return
		}
		close(l.started)

		select {
		case <-l.unblock:
			l.finished = true
		case <-ctx.Done():
		}
	}()

	acc := agents.Accumulator{}
	return acc.ReadStream(ctx, stream, cb)
}

// Stopping a run while the model is streaming ends the turn there: the run
// completes as cancelled rather than waiting for the model to finish.
func TestAgentLoop_StopDuringStreamingEndsTheRun(t *testing.T) {
	llm := &streamingLLM{started: make(chan struct{}), unblock: make(chan struct{})}
	broker := streambroker.NewMemoryStreamBroker()
	defer close(llm.unblock)

	agent := newScriptedAgent("main", llm, nil, broker, nil, nil)

	handle, err := agent.Execute(context.Background(), &agents.AgentInput{
		Namespace: "test",
		ThreadID:  "thread-stream-stop",
		StreamID:  "stream-stop",
		Message:   userMessage("say something long"),
	})
	require.NoError(t, err)

	// Stop once the model is mid-answer.
	<-llm.started
	require.NoError(t, handle.Stop(context.Background()))

	out, err := handle.Result()
	require.NoError(t, err, "a stop is not a run failure")
	requireStatus(t, out, agentstate.RunStatusCompleted)
	assert.Contains(t, messagesText(out.Output), "Cancelled by user")
	assert.False(t, llm.finished, "the model call should have been cut short, not waited on")
}

// The sentinel is what the durable runtimes match on to keep the model from
// being called again, so it must be distinguishable from a tool's.
func TestErrModelCallStopped_IsItsOwnSentinel(t *testing.T) {
	assert.NotErrorIs(t, agents.ErrModelCallStopped, agents.ErrToolCancelled)
	assert.ErrorIs(t, errors.Join(agents.ErrModelCallStopped), agents.ErrModelCallStopped)
}
