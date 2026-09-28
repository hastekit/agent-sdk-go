package history

import (
	"context"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

func pausedCalls(ids ...string) []responses.FunctionCallMessage {
	calls := make([]responses.FunctionCallMessage, 0, len(ids))
	for _, id := range ids {
		calls = append(calls, responses.FunctionCallMessage{CallID: id, Name: "lookup"})
	}
	return calls
}

// outputs lists the tool outputs a run has kept, by call id.
func outputs(run *ConversationRunManager) []string {
	var ids []string
	for _, bundle := range run.newMessages {
		for _, msg := range bundle.Messages {
			if msg.OfFunctionCallOutput != nil {
				ids = append(ids, msg.OfFunctionCallOutput.CallID)
			}
		}
	}
	return ids
}

// Every tool output has to close a call the model made and nothing answered yet.
func TestToolOutputsMustCloseAnOpenCall(t *testing.T) {
	ctx := context.Background()
	run, err := NewRun(ctx, NewConversationManager(NewInMemoryConversationPersistence()), "ns", "thread-open", "")
	require.NoError(t, err)

	run.AddMessages(ctx, userTurn("look it up"))
	run.AddMessages(ctx, withCall(assistantTurn("checking"), "call-1"), AlreadyMeasured())
	require.Equal(t, []string{"call-1"}, run.RunState.OpenToolCalls)

	run.AddMessages(ctx, toolResult("call-1", "found"))
	run.AddMessages(ctx, toolResult("call-1", "found again"))
	run.AddMessages(ctx, toolResult("call-never-made", "made up"))

	require.Equal(t, []string{"call-1"}, outputs(run), "a second output and one for no call are dropped")
	require.Empty(t, run.RunState.OpenToolCalls)
	require.Equal(t, 2, run.staleToolOutputs)
}

// A full-history client replays calls and their outputs together; the calls
// open before the outputs that follow them close them.
func TestReplayedHistoryKeepsItsToolOutputs(t *testing.T) {
	ctx := context.Background()
	run, err := NewRun(ctx, NewConversationManager(NewInMemoryConversationPersistence()), "ns", "thread-replay", "")
	require.NoError(t, err)

	replay := withCall(assistantTurn("checking"), "call-1")
	replay.Messages = append(replay.Messages, toolResult("call-1", "found").Messages...)
	run.AddMessages(ctx, replay)

	require.Equal(t, []string{"call-1"}, outputs(run))
	require.Empty(t, run.RunState.OpenToolCalls)
	require.False(t, run.OnlyStaleToolOutputs())
}

// A paused call stays open in the saved state, so the run that resumes it
// accepts its output — including from rows saved before open calls were tracked.
func TestPausedCallsStayOpenAcrossSaves(t *testing.T) {
	state := agentstate.NewRunState()
	state.OpenToolCall("call-1")
	state.OpenToolCall("call-2")
	require.True(t, state.CloseToolCall("call-1"))
	require.False(t, state.CloseToolCall("call-1"))

	loaded := agentstate.LoadRunStateFromMeta(state.ToMeta())
	require.Equal(t, []string{"call-2"}, loaded.OpenToolCalls)

	// Every call answered stays that way: an emptied list is not refilled
	// from the paused calls, which would let a second output in.
	answered := agentstate.NewRunState()
	answered.TransitionToAwaitApproval(pausedCalls("call-4"))
	require.Empty(t, agentstate.LoadRunStateFromMeta(answered.ToMeta()).OpenToolCalls)

	// An older row: only the paused calls were saved.
	legacy := agentstate.NewRunState()
	legacy.TransitionToAwaitApproval(pausedCalls("call-3"))
	meta := legacy.ToMeta()
	delete(meta["run_state"].(map[string]any), "open_tool_calls")
	require.Equal(t, []string{"call-3"}, agentstate.LoadRunStateFromMeta(meta).OpenToolCalls)
}
