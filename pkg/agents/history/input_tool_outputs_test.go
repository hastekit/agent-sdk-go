package history

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

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

// A tool output in the run's input answers nothing on its own: a waiting tool
// takes its result through the broker, and a paused call takes a resolution.
// The loop's own outputs are not input.
func TestInputToolOutputsNeedTheirCall(t *testing.T) {
	ctx := context.Background()
	run, err := NewRun(ctx, NewConversationManager(NewInMemoryConversationPersistence()), "ns", "thread-input", "")
	require.NoError(t, err)

	run.AddMessages(ctx, toolResult("call-late", "arrived after its run"), AsInput())
	require.Empty(t, outputs(run))
	require.True(t, run.OnlyStaleToolOutputs(), "nothing is left for the model to answer")

	run.AddMessagesToQueue(ctx, []Message{toolResult("call-queued", "folded into the run")})
	require.Empty(t, run.RunState.QueuedMessages)

	run.AddMessages(ctx, withCall(assistantTurn("checking"), "call-1"), AlreadyMeasured())
	run.AddMessages(ctx, toolResult("call-1", "found"))
	require.Equal(t, []string{"call-1"}, outputs(run), "the loop's own output is kept")
	require.Equal(t, 2, run.staleToolOutputs)
}

// A full-history client replays calls with their outputs.
func TestReplayedHistoryKeepsItsToolOutputs(t *testing.T) {
	ctx := context.Background()
	run, err := NewRun(ctx, NewConversationManager(NewInMemoryConversationPersistence()), "ns", "thread-replay", "")
	require.NoError(t, err)

	replay := withCall(assistantTurn("checking"), "call-1")
	replay.Messages = append(replay.Messages, toolResult("call-1", "found").Messages...)
	replay.Messages = append(replay.Messages, toolResult("call-2", "no call").Messages...)
	run.AddMessages(ctx, replay, AsInput())

	require.Equal(t, []string{"call-1"}, outputs(run))
	require.False(t, run.OnlyStaleToolOutputs())
}
