package agui

import (
	"context"
	"slices"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// A client answers its own tools with ordinary tool messages, whichever way
// the agent takes client tool results. A live run keeps the outputs for the
// calls it is waiting on (the stream broker routes them). An idle thread only
// has a use for outputs that answer the client tools its last run paused on:
// they start the run that resumes it. Anything else — a result that arrived
// after its run moved on, or a duplicate — is dropped here rather than
// starting a run with nothing to do.

// onlyToolOutputs reports whether a turn is a client returning tool results.
func onlyToolOutputs(msgs []responses.InputMessageUnion) bool {
	if len(msgs) == 0 {
		return false
	}
	for _, msg := range msgs {
		if msg.OfFunctionCallOutput == nil {
			return false
		}
	}
	return true
}

// answeringToolOutputs keeps the outputs of a tool-only turn that the thread
// has a use for; empty means none.
func answeringToolOutputs(ctx context.Context, agent *agents.Agent, namespace, threadID, streamID string, msgs []responses.InputMessageUnion) ([]responses.InputMessageUnion, error) {
	// A live run decides for itself: its waiting tools take the outputs.
	if active, err := agent.StreamBroker().IsActive(ctx, streamID); err != nil || active {
		return msgs, err
	}
	manager := agent.History()
	if manager == nil || manager.ConversationPersistenceAdapter == nil {
		// No saved state to check against; the run drops what answers nothing.
		return msgs, nil
	}
	page, err := history.LoadTranscriptPage(ctx, manager.ConversationPersistenceAdapter, namespace, threadID, history.TranscriptPageOptions{Limit: 1})
	if err != nil {
		return nil, err
	}
	var pending []string
	if page.Latest != nil {
		if state := agentstate.LoadRunStateFromMeta(page.Latest.Meta); state != nil {
			pending = clientToolCallIDs(state.PendingInterrupts())
		}
	}
	var kept []responses.InputMessageUnion
	for _, msg := range msgs {
		if slices.Contains(pending, msg.OfFunctionCallOutput.CallID) {
			kept = append(kept, msg)
		}
	}
	return kept, nil
}
