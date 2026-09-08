package agents_test

import (
	"context"
	"testing"

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

func (l *llmWithSideEffect) NewStreamingResponses(ctx context.Context, in *responses.Request, cb func(chunk *responses.ResponseChunk)) (*responses.Response, error) {
	l.before(l.inner.callCount())
	return l.inner.NewStreamingResponses(ctx, in, cb)
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
