package history

import (
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// TestProcessInterrupts_NestedBookkeeping verifies the new Interrupt path
// records the same nested-call bookkeeping as the approval path (so
// agent→tool→agent→tool resume works for every mode), and additionally
// stashes the Interrupt record (mode + payload) keyed by call id.
func TestProcessInterrupts_NestedBookkeeping(t *testing.T) {
	cm := &ConversationRunManager{RunState: agentstate.NewRunState()}

	parent := responses.FunctionCallMessage{CallID: "parent_call", Name: "inner_agent"}
	intr := responses.Interrupt{
		FunctionCallMessage: responses.FunctionCallMessage{CallID: "inner_call", Name: "search"},
		Mode:                responses.InterruptModeURL,
	}
	cm.ProcessInterrupts(parent, []responses.Interrupt{intr})

	rs := cm.RunState
	if rs.PendingNestedToolCalls["inner_call"] != "parent_call" {
		t.Fatalf("nested map = %+v, want inner_call→parent_call", rs.PendingNestedToolCalls)
	}
	if _, ok := rs.PausedToolCalls["parent_call"]; !ok {
		t.Fatalf("parent not recorded in PausedToolCalls: %+v", rs.PausedToolCalls)
	}
	got, ok := rs.Interrupts["inner_call"]
	if !ok {
		t.Fatalf("interrupt not recorded for inner_call: %+v", rs.Interrupts)
	}
	if got.Mode != responses.InterruptModeURL || !got.IsNested {
		t.Fatalf("interrupt record = %+v, want mode=url isNested=true", got)
	}
	if len(rs.ToolsAwaitingApproval) != 1 || rs.ToolsAwaitingApproval[0].CallID != "inner_call" {
		t.Fatalf("ToolsAwaitingApproval = %+v", rs.ToolsAwaitingApproval)
	}

	// Promote to await + verify the projection surfaces the nested interrupt.
	rs.PromoteAwaitingToApproval()
	pending := rs.PendingInterrupts()
	if len(pending) != 1 || pending[0].FunctionCallMessage.CallID != "inner_call" {
		t.Fatalf("PendingInterrupts() = %+v", pending)
	}
}

// TestProcessIncomingMessages_ResolutionDrain confirms the generalized
// resolution message drains into the same QueuedApprovals/QueuedRejections
// queues as the legacy approval message, and flips an awaiting run back to
// execute-tools.
func TestProcessIncomingMessages_ResolutionDrain(t *testing.T) {
	cm := &ConversationRunManager{RunState: agentstate.NewRunState()}
	cm.RunState.CurrentStep = agentstate.StepAwaitApproval

	msg := messages.New("user", []responses.InputMessageUnion{
		{
			OfFunctionCallInterruptResolution: &responses.FunctionCallInterruptResolutionMessage{
				ID: "fcir_1",
				Resolutions: []responses.InterruptResolution{
					{CallID: "inner_call", Action: "approve"},
					{CallID: "other_call", Action: "reject"},
				},
			},
		},
	})
	cm.ProcessIncomingMessages(msg, false)

	rs := cm.RunState
	if len(rs.QueuedApprovals) != 1 || rs.QueuedApprovals[0] != "inner_call" {
		t.Fatalf("QueuedApprovals = %+v", rs.QueuedApprovals)
	}
	if len(rs.QueuedRejections) != 1 || rs.QueuedRejections[0] != "other_call" {
		t.Fatalf("QueuedRejections = %+v", rs.QueuedRejections)
	}
	if rs.CurrentStep != agentstate.StepExecuteTools {
		t.Fatalf("step = %q, want execute_tools", rs.CurrentStep)
	}
}

// TestProcessIncomingMessages_DropsResolutionFromMixedBundle pins the fix for
// the AG-UI path: the resolution is bundled with the turn's other messages,
// and it must be drained into the queues — never persisted to history (and
// thus never replayed to the LLM provider, which rejects its type).
func TestProcessIncomingMessages_DropsResolutionFromMixedBundle(t *testing.T) {
	cm := &ConversationRunManager{RunState: agentstate.NewRunState()}
	cm.RunState.CurrentStep = agentstate.StepAwaitApproval

	msg := messages.New("user", []responses.InputMessageUnion{
		{OfFunctionCallInterruptResolution: &responses.FunctionCallInterruptResolutionMessage{
			Resolutions: []responses.InterruptResolution{{CallID: "call_1", Action: "approve"}},
		}},
		{OfInputMessage: &responses.InputMessage{
			Role:    constants.RoleUser,
			Content: responses.InputContent{{OfInputText: &responses.InputTextContent{Text: "hi"}}},
		}},
	})
	cm.ProcessIncomingMessages(msg, false)

	// The decision drained to the queue.
	if len(cm.RunState.QueuedApprovals) != 1 || cm.RunState.QueuedApprovals[0] != "call_1" {
		t.Fatalf("QueuedApprovals = %+v", cm.RunState.QueuedApprovals)
	}
	// History keeps only the real message — no resolution leaks through.
	if len(cm.newMessages) != 1 {
		t.Fatalf("expected 1 stored bundle, got %d", len(cm.newMessages))
	}
	got := cm.newMessages[0].Messages
	if len(got) != 1 || got[0].OfInputMessage == nil {
		t.Fatalf("expected only the user message, got %+v", got)
	}
	if got[0].OfFunctionCallInterruptResolution != nil {
		t.Fatalf("resolution must not be persisted to history")
	}
}
