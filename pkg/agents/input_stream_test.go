package agents_test

import (
	"context"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// inputMessages picks the turns the run announced taking in, in stream order.
func inputMessages(chunks []*responses.ResponseChunk) []*responses.ChunkInputMessage[constants.ChunkTypeInputMessage] {
	var out []*responses.ChunkInputMessage[constants.ChunkTypeInputMessage]
	for _, chunk := range chunks {
		if chunk.OfInputMessage != nil {
			out = append(out, chunk.OfInputMessage)
		}
	}
	return out
}

// The stream used to carry only the agent's half of the exchange, so a client
// replaying it saw answers with the questions missing.
func TestInputMessage_TheTurnThatOpenedTheRunIsOnTheStream(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{textResponse("hello back")}}
	agent := agents.NewAgent(&agents.AgentOptions{Name: "main"}).WithLLM(llm)

	handle, err := agent.Execute(context.Background(), &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-input",
		StreamID: agents.StreamIDForThread("test", "thread-input"),
		Message:  userMessage("hello"),
	})
	require.NoError(t, err)

	said := inputMessages(chunksOf(t, handle))
	require.Len(t, said, 1)
	assert.Equal(t, "hello", said[0].Content)
	assert.Equal(t, string(constants.RoleUser), said[0].Role)
	assert.NotEmpty(t, said[0].MessageID, "a client needs an id to tell an echo from a new turn")
}

// onCall runs a side effect before each scripted response, so a test can make
// something arrive at a chosen point in the run.
type llmWithSideEffect struct {
	inner  *scriptedLLM
	before func(call int)
}

func (l *llmWithSideEffect) NewStreamingResponses(ctx context.Context, call *agents.ModelCall, in *responses.Request, cb func(chunk *responses.ResponseChunk)) (*responses.Response, error) {
	l.before(l.inner.callCount())
	return l.inner.NewStreamingResponses(ctx, call, in, cb)
}

// Where a steering turn appears is the whole point.
//
// This one arrives while the first model call is in flight, so the loop takes
// it off the broker at the next boundary — before the tool runs — and only
// folds it into the conversation at the boundary after. Between those two
// moments the tool call runs and its result goes out. Announcing the turn when
// it was taken off the broker would put it ahead of that result and tell the
// client the user interrupted before work they had in fact already seen.
func TestInputMessage_ASteeringTurnIsAnnouncedWhereItWasPickedUp(t *testing.T) {
	const streamID = "steer-stream"
	broker := streambroker.NewMemoryStreamBroker()

	scripted := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_gather", "gather", "{}"),
		textResponse("final answer"),
	}}
	llm := &llmWithSideEffect{inner: scripted, before: func(call int) {
		if call != 0 {
			return
		}
		require.NoError(t, broker.EnqueueMessage(context.Background(), streamID,
			messages.New("alice", []responses.InputMessageUnion{{
				OfEasyInput: &responses.EasyMessage{
					Role:    constants.RoleUser,
					Content: responses.EasyInputContentUnion{OfString: utils.Ptr("actually, hurry up")},
				},
			}})))
	}}
	gather := newFakeTool("gather", false, "gather done")
	agent := newScriptedAgent("main", llm, nil, broker, []agents.Tool{gather}, nil)

	handle, err := agent.Execute(context.Background(), &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-steer", StreamID: streamID,
		Message: userMessage("start working"),
	})
	require.NoError(t, err)
	chunks := chunksOf(t, handle)

	said := inputMessages(chunks)
	require.Len(t, said, 2)
	assert.Equal(t, "start working", said[0].Content)
	assert.Equal(t, "actually, hurry up", said[1].Content)
	assert.Equal(t, "alice", said[1].SenderID, "attributed to whoever sent it")

	steerAt := indexOfInputMessage(t, chunks, "actually, hurry up")
	resultAt := indexOfToolResult(t, chunks, "call_gather")
	assert.Less(t, resultAt, steerAt,
		"the turn arrived during the tool call, so it lands after that call's result")
}

// enqueueDuringFirstCall makes a turn reach the queue while the first model
// call is in flight, then runs the agent to its end.
func enqueueDuringFirstCall(t *testing.T, opts *agents.AgentOptions, scripted *scriptedLLM, text string) (*agents.AgentOutput, []*responses.ResponseChunk) {
	t.Helper()
	const streamID = "final-call-stream"
	broker := streambroker.NewMemoryStreamBroker()
	opts.StreamBroker = broker
	agent := agents.NewAgent(opts).WithLLM(&llmWithSideEffect{inner: scripted, before: func(call int) {
		if call != 0 {
			return
		}
		require.NoError(t, broker.EnqueueMessage(context.Background(), streamID,
			messages.New("alice", []responses.InputMessageUnion{{
				OfEasyInput: &responses.EasyMessage{
					Role:    constants.RoleUser,
					Content: responses.EasyInputContentUnion{OfString: utils.Ptr(text)},
				},
			}})))
	}})
	handle, err := agent.Execute(context.Background(), &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-final-call", StreamID: streamID,
		Message: userMessage("start working"),
	})
	require.NoError(t, err)
	chunks := chunksOf(t, handle)
	out, err := handle.Wait()
	require.NoError(t, err)
	return out, chunks
}

// A model call that uses no tools is the run's last, so nothing used to look at
// the queue after it. A turn arriving during that call was acknowledged to its
// sender and then discarded by the next run — and this is the call users are
// most likely to interrupt, since it is the answer they are watching appear.
func TestSteering_ATurnArrivingDuringTheFinalModelCallIsAnswered(t *testing.T) {
	scripted := &scriptedLLM{script: []*responses.Response{
		textResponse("first answer"),
		textResponse("second answer"),
	}}
	out, chunks := enqueueDuringFirstCall(t, &agents.AgentOptions{Name: "main"}, scripted, "one more thing")

	assert.Equal(t, agentstate.RunStatusCompleted, out.Status)
	require.Equal(t, 2, scripted.callCount(), "the run goes round again for the turn")
	input, err := sonic.Marshal(scripted.request(1).Input)
	require.NoError(t, err)
	assert.Contains(t, string(input), "one more thing")
	assert.Contains(t, string(input), "first answer", "the turn follows the reply it arrived during")

	said := inputMessages(chunks)
	require.Len(t, said, 2)
	assert.Equal(t, "one more thing", said[1].Content)
}

// Going round again costs a loop. A run on its last one has still succeeded, and
// must not turn into a failure because someone spoke while it was finishing.
func TestSteering_ARunOutOfLoopsCompletesRatherThanFails(t *testing.T) {
	scripted := &scriptedLLM{script: []*responses.Response{textResponse("only answer")}}
	out, chunks := enqueueDuringFirstCall(t, &agents.AgentOptions{Name: "main", MaxLoops: utils.Ptr(1)}, scripted, "too late")

	assert.Equal(t, agentstate.RunStatusCompleted, out.Status)
	assert.Equal(t, 1, scripted.callCount())
	assert.Len(t, inputMessages(chunks), 1, "the turn was never taken in, so it is not announced")
}

// A single-turn agent ends as soon as the model has responded; that is its contract.
func TestSteering_ASingleTurnAgentDoesNotGoRound(t *testing.T) {
	scripted := &scriptedLLM{script: []*responses.Response{textResponse("only answer")}}
	out, chunks := enqueueDuringFirstCall(t, &agents.AgentOptions{Name: "main", SingleTurn: true}, scripted, "ignored")

	assert.Equal(t, agentstate.RunStatusCompleted, out.Status)
	assert.Equal(t, 1, scripted.callCount())
	assert.Len(t, inputMessages(chunks), 1)
}

// A stop asked for before the model finished wins over a turn queued alongside it.
func TestSteering_AStopWinsOverAQueuedTurn(t *testing.T) {
	scripted := &scriptedLLM{script: []*responses.Response{textResponse("only answer")}}
	broker := streambroker.NewMemoryStreamBroker()
	const streamID = "stop-final-call"
	agent := agents.NewAgent(&agents.AgentOptions{Name: "main", StreamBroker: broker}).WithLLM(&llmWithSideEffect{inner: scripted, before: func(call int) {
		if call != 0 {
			return
		}
		ctx := context.Background()
		require.NoError(t, broker.EnqueueMessage(ctx, streamID, userMessage("never mind")))
		require.NoError(t, broker.Stop(ctx, streamID))
	}})
	handle, err := agent.Execute(context.Background(), &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-stop-final-call", StreamID: streamID,
		Message: userMessage("start working"),
	})
	require.NoError(t, err)
	chunks := chunksOf(t, handle)
	out, err := handle.Wait()
	require.NoError(t, err)

	assert.Equal(t, agentstate.RunStatusCompleted, out.Status)
	assert.Equal(t, 1, scripted.callCount(), "no further model call after the stop")
	assert.Len(t, inputMessages(chunks), 1, "the queued turn was never taken in")
}

// A task's result is delivered as a user turn — the only shape a provider
// takes it in — but nobody wrote it, so it is not announced as something
// somebody said.
func TestInputMessage_ABackgroundResultIsNotAnnouncedAsATurn(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{textResponse("thanks")}}
	agent := agents.NewAgent(&agents.AgentOptions{Name: "main"}).WithLLM(llm)

	delivery := messages.New("", []responses.InputMessageUnion{{
		OfEasyInput: &responses.EasyMessage{
			Role:    constants.RoleUser,
			Content: responses.EasyInputContentUnion{OfString: utils.Ptr("[Background task task-1 has finished.]")},
		},
	}})
	delivery.BackgroundTaskID = "task-1"

	handle, err := agent.Execute(context.Background(), &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-bg-input",
		StreamID: agents.StreamIDForThread("test", "thread-bg-input"),
		Message:  delivery,
	})
	require.NoError(t, err)

	out, err := handle.Wait()
	for range handle.Chunks {
	}
	require.NoError(t, err)
	require.Equal(t, agentstate.RunStatusCompleted, out.Status)
}

func indexOfInputMessage(t *testing.T, chunks []*responses.ResponseChunk, text string) int {
	t.Helper()
	for i, chunk := range chunks {
		if chunk.OfInputMessage != nil && chunk.OfInputMessage.Content == text {
			return i
		}
	}
	t.Fatalf("no input message chunk carrying %q", text)
	return -1
}

func indexOfToolResult(t *testing.T, chunks []*responses.ResponseChunk, callID string) int {
	t.Helper()
	for i, chunk := range chunks {
		if chunk.OfFunctionCallOutput != nil && chunk.OfFunctionCallOutput.CallID == callID {
			return i
		}
	}
	t.Fatalf("no function call output for %q", callID)
	return -1
}
