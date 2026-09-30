package agui

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// A client answers its own tools with ordinary tool messages, whichever way
// the agent takes client tool results. A live run keeps the outputs for the
// calls it is waiting on (the stream broker routes them to the waiting tool).
// An idle thread only has a use for outputs that answer the client tools its
// last run paused on, and the agent resumes a pause with a resolution, not an
// output: resolveClientTools turns them into one. Anything else — a result
// that arrived after its run moved on, or a duplicate — is dropped here rather
// than starting a run with nothing to do.

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
	pending, known, err := pausedClientCalls(ctx, agent, namespace, threadID)
	if err != nil || !known {
		// Without saved state to check against, the run drops what answers nothing.
		return msgs, err
	}
	var kept []responses.InputMessageUnion
	for _, msg := range msgs {
		if slices.Contains(pending, msg.OfFunctionCallOutput.CallID) {
			kept = append(kept, msg)
		}
	}
	return kept, nil
}

// pausedClientCalls names the client tool calls the thread's last run paused
// on. known is false when the agent keeps no state to read them from.
func pausedClientCalls(ctx context.Context, agent *agents.Agent, namespace, threadID string) (calls []string, known bool, err error) {
	manager := agent.History()
	if manager == nil || manager.ConversationPersistenceAdapter == nil {
		return nil, false, nil
	}
	page, err := history.LoadTranscriptPage(ctx, manager.ConversationPersistenceAdapter, namespace, threadID, history.TranscriptPageOptions{Limit: 1})
	if err != nil {
		return nil, true, err
	}
	if page.Latest != nil {
		if state := agentstate.LoadRunStateFromMeta(page.Latest.Meta); state != nil {
			calls = clientToolCallIDs(state.PendingInterrupts())
		}
	}
	return calls, true, nil
}

// resolveClientTools turns the outputs in a turn that answer client tools the
// thread is paused on into the resolutions that resume them, with each result
// as its resolution's content. Other outputs are left for the agent, which
// drops those that answer nothing.
//
// It runs once the handler holds the thread, never before: a run that was
// finishing when the turn arrived has paused by then, and the pause it left is
// the one read here.
func resolveClientTools(ctx context.Context, agent *agents.Agent, namespace, threadID string, msgs []responses.InputMessageUnion) ([]responses.InputMessageUnion, error) {
	if !slices.ContainsFunc(msgs, func(m responses.InputMessageUnion) bool { return m.OfFunctionCallOutput != nil }) {
		return msgs, nil
	}
	paused, _, err := pausedClientCalls(ctx, agent, namespace, threadID)
	if err != nil || len(paused) == 0 {
		return msgs, err
	}
	var resolutions []responses.InterruptResolution
	var rest []responses.InputMessageUnion
	for _, msg := range msgs {
		out := msg.OfFunctionCallOutput
		if out == nil || !slices.Contains(paused, out.CallID) {
			rest = append(rest, msg)
			continue
		}
		content, err := toolOutputContent(out)
		if err != nil {
			return nil, err
		}
		resolutions = append(resolutions, responses.InterruptResolution{
			CallID:  out.CallID,
			Action:  responses.InterruptActionApprove,
			Content: content,
		})
	}
	if len(resolutions) == 0 {
		return msgs, nil
	}
	resolution := responses.InputMessageUnion{OfFunctionCallInterruptResolution: &responses.FunctionCallInterruptResolutionMessage{
		Resolutions: resolutions,
	}}
	return append([]responses.InputMessageUnion{resolution}, rest...), nil
}

// toolOutputContent is a tool output as resolution content: text as a JSON
// string, multipart output as its JSON.
func toolOutputContent(out *responses.FunctionCallOutputMessage) (json.RawMessage, error) {
	if out.Output.OfString != nil {
		return json.Marshal(*out.Output.OfString)
	}
	return json.Marshal(out.Output.OfList)
}
