package agents_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/hastekit/hastekit-sdk-go/pkg/agents"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/history"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/messages"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/streambroker"
	agenttools "github.com/hastekit/hastekit-sdk-go/pkg/agents/tools"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/hastekit-sdk-go/pkg/utils"
)

// scriptedLLM returns one canned response per call, in order, and
// records every request so tests can assert on the conversation the
// loop actually sent to the model.
type scriptedLLM struct {
	mu       sync.Mutex
	script   []*responses.Response
	requests []*responses.Request
}

func (s *scriptedLLM) NewStreamingResponses(ctx context.Context, in *responses.Request, cb func(chunk *responses.ResponseChunk)) (*responses.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, in)
	if len(s.requests) > len(s.script) {
		return nil, fmt.Errorf("scripted LLM exhausted: call %d but only %d responses scripted", len(s.requests), len(s.script))
	}
	return s.script[len(s.requests)-1], nil
}

func (s *scriptedLLM) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *scriptedLLM) request(i int) *responses.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[i]
}

// fakeTool counts executions and returns a fixed text output. Tests can
// override execute to hook side effects (stop signals, queued messages).
type fakeTool struct {
	*agents.BaseTool
	mu      sync.Mutex
	calls   int
	execute func(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error)
}

func newFakeTool(name string, requiresApproval bool, output string) *fakeTool {
	t := &fakeTool{
		BaseTool: &agents.BaseTool{
			ToolUnion: responses.ToolUnion{
				OfFunction: &responses.FunctionTool{
					Name:        name,
					Description: utils.Ptr("test tool"),
					Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
				},
			},
			RequiresApproval: requiresApproval,
		},
	}
	t.execute = func(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
		return &agents.ToolCallResponse{
			FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
				ID:     params.ID,
				CallID: params.CallID,
				Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr(output)},
			},
		}, nil
	}
	return t
}

func (t *fakeTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	t.mu.Lock()
	t.calls++
	t.mu.Unlock()
	return t.execute(ctx, params)
}

func (t *fakeTool) callCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

func textResponse(text string) *responses.Response {
	return &responses.Response{
		Output: []responses.OutputMessageUnion{{
			OfOutputMessage: &responses.OutputMessage{
				ID:   responses.NewOutputItemMessageID(),
				Role: constants.RoleAssistant,
				Content: &responses.OutputContent{
					{OfOutputText: &responses.OutputTextContent{Text: text}},
				},
			},
		}},
	}
}

func toolCallResponse(callID, name, args string) *responses.Response {
	return &responses.Response{
		Output: []responses.OutputMessageUnion{{
			OfFunctionCall: &responses.FunctionCallMessage{
				ID:        "fc_" + callID,
				CallID:    callID,
				Name:      name,
				Arguments: args,
			},
		}},
	}
}

func userMessage(text string) history.Message {
	return messages.New("user", []responses.InputMessageUnion{{
		OfEasyInput: &responses.EasyMessage{
			Role:    constants.RoleUser,
			Content: responses.EasyInputContentUnion{OfString: utils.Ptr(text)},
		},
	}})
}

func approvalMessage(approved, rejected []string) history.Message {
	resolutions := make([]responses.InterruptResolution, 0, len(approved)+len(rejected))
	for _, id := range approved {
		resolutions = append(resolutions, responses.InterruptResolution{CallID: id, Action: responses.InterruptActionApprove})
	}
	for _, id := range rejected {
		resolutions = append(resolutions, responses.InterruptResolution{CallID: id, Action: responses.InterruptActionReject})
	}
	return messages.New("user", []responses.InputMessageUnion{{
		OfFunctionCallInterruptResolution: &responses.FunctionCallInterruptResolutionMessage{
			Resolutions: resolutions,
		},
	}})
}

func newScriptedAgent(name string, llm agents.LLM, hist *history.CommonConversationManager, broker agents.StreamBroker, tools []agents.Tool, handoffs []*agents.Handoff) *agents.Agent {
	return agents.NewAgent(&agents.AgentOptions{
		Name:         name,
		History:      hist,
		StreamBroker: broker,
		Tools:        tools,
		Handoffs:     handoffs,
	}).WithLLM(llm)
}

func runAgent(t *testing.T, agent *agents.Agent, in *agents.AgentInput) *agents.AgentOutput {
	t.Helper()
	handle, err := agent.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	out, err := handle.Result()
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	return out
}

// messagesText flattens the text carried by a message list — assistant
// text, user text, and tool outputs — for substring assertions.
func messagesText(msgs []responses.InputMessageUnion) string {
	var b strings.Builder
	for _, m := range msgs {
		switch {
		case m.OfOutputMessage != nil && m.OfOutputMessage.Content != nil:
			for _, c := range *m.OfOutputMessage.Content {
				if c.OfOutputText != nil {
					b.WriteString(c.OfOutputText.Text)
				}
			}
		case m.OfInputMessage != nil:
			for _, c := range m.OfInputMessage.Content {
				if c.OfInputText != nil {
					b.WriteString(c.OfInputText.Text)
				}
			}
		case m.OfEasyInput != nil && m.OfEasyInput.Content.OfString != nil:
			b.WriteString(*m.OfEasyInput.Content.OfString)
		case m.OfFunctionCallOutput != nil && m.OfFunctionCallOutput.Output.OfString != nil:
			b.WriteString(*m.OfFunctionCallOutput.Output.OfString)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func requireStatus(t *testing.T, out *agents.AgentOutput, want agentstate.RunStatus) {
	t.Helper()
	if out.Status != want {
		t.Fatalf("run status = %q, want %q", out.Status, want)
	}
}

func requireSinglePendingApproval(t *testing.T, out *agents.AgentOutput, toolName, callID string) {
	t.Helper()
	if len(out.Interrupts) != 1 {
		t.Fatalf("interrupts = %d, want 1: %+v", len(out.Interrupts), out.Interrupts)
	}
	intr := out.Interrupts[0]
	if intr.Mode != responses.InterruptModeApproval {
		t.Fatalf("interrupt mode = %q, want approval", intr.Mode)
	}
	pa := intr.FunctionCallMessage
	if pa.Name != toolName || pa.CallID != callID {
		t.Fatalf("pending approval = %s/%s, want %s/%s", pa.Name, pa.CallID, toolName, callID)
	}
}

func TestAgentLoop_PauseOnApproval_DeclineSkipsTool(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_danger", "dangerous", "{}"),
		textResponse("acknowledged the decline"),
	}}
	danger := newFakeTool("dangerous", true, "dangerous done")
	agent := newScriptedAgent("main", llm, nil, nil, []agents.Tool{danger}, nil)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test",
		ThreadID:  "thread-decline",
		Message:   userMessage("please do something dangerous"),
	})

	requireStatus(t, out, agentstate.RunStatusPaused)
	requireSinglePendingApproval(t, out, "dangerous", "call_danger")
	if danger.callCount() != 0 {
		t.Fatalf("tool executed %d times before approval, want 0", danger.callCount())
	}
	if llm.callCount() != 1 {
		t.Fatalf("LLM called %d times before approval, want 1", llm.callCount())
	}

	// Decline the tool call and resume the paused run.
	out = runAgent(t, agent, &agents.AgentInput{
		Namespace:         "test",
		ThreadID:          "thread-decline",
		PreviousMessageID: out.RunID,
		Message:           approvalMessage(nil, []string{"call_danger"}),
	})

	requireStatus(t, out, agentstate.RunStatusCompleted)
	if danger.callCount() != 0 {
		t.Fatalf("declined tool executed %d times, want 0", danger.callCount())
	}
	if text := messagesText(out.Output); !strings.Contains(text, "User has declined") {
		t.Fatalf("output missing decline tool result, got:\n%s", text)
	}
	if llm.callCount() != 2 {
		t.Fatalf("LLM called %d times, want 2", llm.callCount())
	}
}

func TestAgentLoop_PauseOnApproval_ApproveExecutesTool(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_danger", "dangerous", "{}"),
		textResponse("done"),
	}}
	danger := newFakeTool("dangerous", true, "dangerous done")
	agent := newScriptedAgent("main", llm, nil, nil, []agents.Tool{danger}, nil)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test",
		ThreadID:  "thread-approve",
		Message:   userMessage("please do something dangerous"),
	})
	requireStatus(t, out, agentstate.RunStatusPaused)

	out = runAgent(t, agent, &agents.AgentInput{
		Namespace:         "test",
		ThreadID:          "thread-approve",
		PreviousMessageID: out.RunID,
		Message:           approvalMessage([]string{"call_danger"}, nil),
	})

	requireStatus(t, out, agentstate.RunStatusCompleted)
	if danger.callCount() != 1 {
		t.Fatalf("approved tool executed %d times, want 1", danger.callCount())
	}
	if text := messagesText(out.Output); !strings.Contains(text, "dangerous done") {
		t.Fatalf("output missing tool result, got:\n%s", text)
	}
}

func TestAgentLoop_StopSignalEndsRun(t *testing.T) {
	const streamID = "stop-stream"
	broker := streambroker.NewMemoryStreamBroker()

	llm := &scriptedLLM{script: []*responses.Response{
		// Only one response scripted: if the loop survives the stop
		// signal and calls the LLM again, the scripted LLM errors.
		toolCallResponse("call_stop", "stopper", "{}"),
	}}
	stopper := newFakeTool("stopper", false, "stopper done")
	innerExecute := stopper.execute
	stopper.execute = func(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
		// Signal a stop mid-run; the loop should honor it at the next
		// iteration boundary instead of calling the LLM again.
		if err := broker.Stop(ctx, streamID); err != nil {
			return nil, err
		}
		return innerExecute(ctx, params)
	}
	agent := newScriptedAgent("main", llm, nil, broker, []agents.Tool{stopper}, nil)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test",
		ThreadID:  "thread-stop",
		StreamID:  streamID,
		Message:   userMessage("start working"),
	})

	requireStatus(t, out, agentstate.RunStatusCompleted)
	if llm.callCount() != 1 {
		t.Fatalf("LLM called %d times after stop, want 1", llm.callCount())
	}
	if stopper.callCount() != 1 {
		t.Fatalf("tool executed %d times, want 1", stopper.callCount())
	}
	if text := messagesText(out.Output); !strings.Contains(text, "Cancelled by user") {
		t.Fatalf("output missing cancellation message, got:\n%s", text)
	}
}

func TestAgentLoop_QueuedMessagesInjectedAtIterationBoundary(t *testing.T) {
	const streamID = "queue-stream"
	broker := streambroker.NewMemoryStreamBroker()

	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_gather", "gather", "{}"),
		textResponse("final answer"),
	}}
	gather := newFakeTool("gather", false, "gather done")
	innerExecute := gather.execute
	gather.execute = func(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
		// A message arrives while the loop is busy executing tools. It
		// must be drained at the next boundary and injected into the
		// following LLM call.
		err := broker.EnqueueMessage(ctx, streamID, messages.New("alice", []responses.InputMessageUnion{{
			OfEasyInput: &responses.EasyMessage{
				Role:    constants.RoleUser,
				Content: responses.EasyInputContentUnion{OfString: utils.Ptr("extra info from alice")},
			},
		}}))
		if err != nil {
			return nil, err
		}
		return innerExecute(ctx, params)
	}
	agent := newScriptedAgent("main", llm, nil, broker, []agents.Tool{gather}, nil)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test",
		ThreadID:  "thread-queue",
		StreamID:  streamID,
		Message:   userMessage("start working"),
	})

	requireStatus(t, out, agentstate.RunStatusCompleted)
	if llm.callCount() != 2 {
		t.Fatalf("LLM called %d times, want 2", llm.callCount())
	}

	first := messagesText(llm.request(0).Input.OfInputMessageList)
	if strings.Contains(first, "extra info from alice") {
		t.Fatalf("queued message leaked into the first LLM call:\n%s", first)
	}
	second := messagesText(llm.request(1).Input.OfInputMessageList)
	if !strings.Contains(second, "extra info from alice") {
		t.Fatalf("queued message not injected into the second LLM call:\n%s", second)
	}
	// The tool result of the in-flight call must still precede the
	// injected message — queued input slots in at the boundary, not
	// in the middle of a tool turn.
	if strings.Index(second, "gather done") > strings.Index(second, "extra info from alice") {
		t.Fatalf("queued message injected before the pending tool result:\n%s", second)
	}
}

func TestAgentLoop_SubAgentApprovalPausesParent(t *testing.T) {
	childLLM := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_child_danger", "child_danger", "{}"),
		textResponse("child finished"),
	}}
	childDanger := newFakeTool("child_danger", true, "child danger done")
	child := newScriptedAgent("child", childLLM, nil, nil, []agents.Tool{childDanger}, nil)

	parentLLM := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_delegate", "child_agent", `{"message":"do it"}`),
		textResponse("parent finished"),
	}}
	parent := newScriptedAgent("parent", parentLLM, nil, nil, []agents.Tool{
		agenttools.NewAgentTool("child_agent", "delegate to the child agent", child, agenttools.SubAgentContextModeIsolated),
	}, nil)

	out := runAgent(t, parent, &agents.AgentInput{
		Namespace: "test",
		ThreadID:  "thread-subagent",
		Message:   userMessage("delegate this"),
	})

	// The child's approval requirement must surface as a pause of the
	// parent run, carrying the child's pending tool call.
	requireStatus(t, out, agentstate.RunStatusPaused)
	requireSinglePendingApproval(t, out, "child_danger", "call_child_danger")
	if childDanger.callCount() != 0 {
		t.Fatalf("child tool executed %d times before approval, want 0", childDanger.callCount())
	}

	out = runAgent(t, parent, &agents.AgentInput{
		Namespace:         "test",
		ThreadID:          "thread-subagent",
		PreviousMessageID: out.RunID,
		Message:           approvalMessage([]string{"call_child_danger"}, nil),
	})

	requireStatus(t, out, agentstate.RunStatusCompleted)
	if childDanger.callCount() != 1 {
		t.Fatalf("child tool executed %d times after approval, want 1", childDanger.callCount())
	}
	if childLLM.callCount() != 2 {
		t.Fatalf("child LLM called %d times, want 2", childLLM.callCount())
	}
	if parentLLM.callCount() != 2 {
		t.Fatalf("parent LLM called %d times, want 2", parentLLM.callCount())
	}
	text := messagesText(out.Output)
	if !strings.Contains(text, "child finished") {
		t.Fatalf("output missing sub-agent result, got:\n%s", text)
	}
	if !strings.Contains(text, "parent finished") {
		t.Fatalf("output missing parent answer, got:\n%s", text)
	}
}

func TestAgentLoop_SubSubAgentApprovalPausesParent(t *testing.T) {
	grandchildLLM := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_gc_danger", "gc_danger", "{}"),
		textResponse("grandchild finished"),
	}}
	gcDanger := newFakeTool("gc_danger", true, "gc danger done")
	grandchild := newScriptedAgent("grandchild", grandchildLLM, nil, nil, []agents.Tool{gcDanger}, nil)

	childLLM := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_gc_agent", "gc_agent", `{"message":"go deeper"}`),
		textResponse("child finished"),
	}}
	child := newScriptedAgent("child", childLLM, nil, nil, []agents.Tool{
		agenttools.NewAgentTool("gc_agent", "delegate to the grandchild agent", grandchild, agenttools.SubAgentContextModeIsolated),
	}, nil)

	parentLLM := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_child_agent", "child_agent", `{"message":"do it"}`),
		textResponse("parent finished"),
	}}
	parent := newScriptedAgent("parent", parentLLM, nil, nil, []agents.Tool{
		agenttools.NewAgentTool("child_agent", "delegate to the child agent", child, agenttools.SubAgentContextModeIsolated),
	}, nil)

	out := runAgent(t, parent, &agents.AgentInput{
		Namespace: "test",
		ThreadID:  "thread-subsubagent",
		Message:   userMessage("delegate this twice"),
	})

	// The grandchild's approval requirement must cascade through both
	// levels and pause the top-level run.
	requireStatus(t, out, agentstate.RunStatusPaused)
	requireSinglePendingApproval(t, out, "gc_danger", "call_gc_danger")
	if gcDanger.callCount() != 0 {
		t.Fatalf("grandchild tool executed %d times before approval, want 0", gcDanger.callCount())
	}

	out = runAgent(t, parent, &agents.AgentInput{
		Namespace:         "test",
		ThreadID:          "thread-subsubagent",
		PreviousMessageID: out.RunID,
		Message:           approvalMessage([]string{"call_gc_danger"}, nil),
	})

	requireStatus(t, out, agentstate.RunStatusCompleted)
	if gcDanger.callCount() != 1 {
		t.Fatalf("grandchild tool executed %d times after approval, want 1", gcDanger.callCount())
	}
	// Each level resumes exactly once: one more LLM call apiece.
	for name, llm := range map[string]*scriptedLLM{"grandchild": grandchildLLM, "child": childLLM, "parent": parentLLM} {
		if llm.callCount() != 2 {
			t.Fatalf("%s LLM called %d times, want 2", name, llm.callCount())
		}
	}
	if text := messagesText(out.Output); !strings.Contains(text, "parent finished") {
		t.Fatalf("output missing parent answer, got:\n%s", text)
	}
}

func TestAgentLoop_HandoffToolApprovalPausesRun(t *testing.T) {
	persistence := history.NewInMemoryConversationPersistence()
	hist := history.NewConversationManager(persistence)
	broker := streambroker.NewMemoryStreamBroker()

	bLLM := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_b_danger", "b_danger", "{}"),
		textResponse("agent-b finished"),
	}}
	bDanger := newFakeTool("b_danger", true, "b danger done")
	agentB := newScriptedAgent("agent-b", bLLM, hist, broker, []agents.Tool{bDanger}, nil)

	aLLM := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_transfer", "transfer_to_agent", `{"agent_name":"agent-b"}`),
	}}
	agentA := newScriptedAgent("agent-a", aLLM, hist, broker, nil, []*agents.Handoff{
		agents.NewHandoff("agent-b", "handles part b", agentB),
	})

	out := runAgent(t, agentA, &agents.AgentInput{
		Namespace: "test",
		ThreadID:  "thread-handoff",
		Message:   userMessage("transfer me"),
	})

	// The handoff target's approval requirement pauses the shared run.
	requireStatus(t, out, agentstate.RunStatusPaused)
	requireSinglePendingApproval(t, out, "b_danger", "call_b_danger")
	if bDanger.callCount() != 0 {
		t.Fatalf("handoff tool executed %d times before approval, want 0", bDanger.callCount())
	}

	// The paused run records the handoff target as the last active
	// agent, which is how a host routes the resume.
	saved, err := persistence.LoadMessages(context.Background(), "test", "thread-handoff", out.RunID)
	if err != nil || len(saved) == 0 {
		t.Fatalf("failed to load persisted run: %v", err)
	}
	runState := agentstate.LoadRunStateFromMeta(saved[len(saved)-1].Meta)
	if runState == nil || runState.LastAgentName != "agent-b" {
		t.Fatalf("persisted last agent = %+v, want agent-b", runState)
	}

	// Resume on the handoff target, as the host would.
	out = runAgent(t, agentB, &agents.AgentInput{
		Namespace:         "test",
		ThreadID:          "thread-handoff",
		PreviousMessageID: out.RunID,
		Message:           approvalMessage([]string{"call_b_danger"}, nil),
	})

	requireStatus(t, out, agentstate.RunStatusCompleted)
	if bDanger.callCount() != 1 {
		t.Fatalf("handoff tool executed %d times after approval, want 1", bDanger.callCount())
	}
	if aLLM.callCount() != 1 {
		t.Fatalf("agent-a LLM called %d times, want 1", aLLM.callCount())
	}
	if bLLM.callCount() != 2 {
		t.Fatalf("agent-b LLM called %d times, want 2", bLLM.callCount())
	}
	if text := messagesText(out.Output); !strings.Contains(text, "agent-b finished") {
		t.Fatalf("output missing handoff agent answer, got:\n%s", text)
	}
}

func TestAgentLoop_StickyHandoffRoutesNextTurnToSpecialist(t *testing.T) {
	persistence := history.NewInMemoryConversationPersistence()
	hist := history.NewConversationManager(persistence)
	broker := streambroker.NewMemoryStreamBroker()

	bLLM := &scriptedLLM{script: []*responses.Response{
		textResponse("agent-b turn 1"),
		textResponse("agent-b turn 2"),
	}}
	agentB := newScriptedAgent("agent-b", bLLM, hist, broker, nil, nil)

	// The root only ever transfers — a single scripted response. If turn 2
	// re-entered the root, the scripted LLM would be exhausted and error.
	aLLM := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_transfer", "transfer_to_agent", `{"agent_name":"agent-b"}`),
	}}
	agentA := agents.NewAgent(&agents.AgentOptions{
		Name:          "agent-a",
		History:       hist,
		StreamBroker:  broker,
		StickyHandoff: true,
		Handoffs: []*agents.Handoff{
			agents.NewHandoff("agent-b", "handles part b", agentB),
		},
	}).WithLLM(aLLM)

	// Turn 1: root hands off to the specialist.
	out := runAgent(t, agentA, &agents.AgentInput{
		Namespace: "test",
		ThreadID:  "thread-sticky",
		Message:   userMessage("transfer me"),
	})
	requireStatus(t, out, agentstate.RunStatusCompleted)
	if text := messagesText(out.Output); !strings.Contains(text, "agent-b turn 1") {
		t.Fatalf("turn 1 output missing specialist answer, got:\n%s", text)
	}

	// Turn 2: same thread, entering through the ROOT again. Sticky routing
	// must send it straight to the specialist without re-running the root.
	out = runAgent(t, agentA, &agents.AgentInput{
		Namespace:         "test",
		ThreadID:          "thread-sticky",
		PreviousMessageID: out.RunID,
		Message:           userMessage("and again"),
	})
	requireStatus(t, out, agentstate.RunStatusCompleted)

	if aLLM.callCount() != 1 {
		t.Fatalf("root LLM called %d times, want 1 (turn 2 must skip the root)", aLLM.callCount())
	}
	if bLLM.callCount() != 2 {
		t.Fatalf("specialist LLM called %d times, want 2", bLLM.callCount())
	}
	if text := messagesText(out.Output); !strings.Contains(text, "agent-b turn 2") {
		t.Fatalf("turn 2 output missing specialist answer, got:\n%s", text)
	}
}

// Without the sticky flag, a subsequent turn re-enters the root agent —
// the pre-existing behavior the flag opts out of.
func TestAgentLoop_NonStickyHandoffReentersRootNextTurn(t *testing.T) {
	persistence := history.NewInMemoryConversationPersistence()
	hist := history.NewConversationManager(persistence)
	broker := streambroker.NewMemoryStreamBroker()

	bLLM := &scriptedLLM{script: []*responses.Response{
		textResponse("agent-b turn 1"),
	}}
	agentB := newScriptedAgent("agent-b", bLLM, hist, broker, nil, nil)

	aLLM := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_transfer", "transfer_to_agent", `{"agent_name":"agent-b"}`),
		textResponse("agent-a turn 2"),
	}}
	// newScriptedAgent leaves StickyHandoff at its false default.
	agentA := newScriptedAgent("agent-a", aLLM, hist, broker, nil, []*agents.Handoff{
		agents.NewHandoff("agent-b", "handles part b", agentB),
	})

	out := runAgent(t, agentA, &agents.AgentInput{
		Namespace: "test",
		ThreadID:  "thread-nonsticky",
		Message:   userMessage("transfer me"),
	})
	requireStatus(t, out, agentstate.RunStatusCompleted)

	out = runAgent(t, agentA, &agents.AgentInput{
		Namespace:         "test",
		ThreadID:          "thread-nonsticky",
		PreviousMessageID: out.RunID,
		Message:           userMessage("and again"),
	})
	requireStatus(t, out, agentstate.RunStatusCompleted)

	// Turn 2 went back to the root: root LLM ran twice, specialist once.
	if aLLM.callCount() != 2 {
		t.Fatalf("root LLM called %d times, want 2 (turn 2 re-enters the root)", aLLM.callCount())
	}
	if bLLM.callCount() != 1 {
		t.Fatalf("specialist LLM called %d times, want 1", bLLM.callCount())
	}
	if text := messagesText(out.Output); !strings.Contains(text, "agent-a turn 2") {
		t.Fatalf("turn 2 output missing root answer, got:\n%s", text)
	}
}

// Back-handoff: a specialist transfers control back to the root, which
// (under sticky routing) un-sticks the thread so later turns re-enter the
// root. The reverse edge is wired with AddHandoffs after both agents
// exist, since root ↔ specialist is a construction-time cycle.
func TestAgentLoop_StickyBackHandoffUnsticksThread(t *testing.T) {
	persistence := history.NewInMemoryConversationPersistence()
	hist := history.NewConversationManager(persistence)
	broker := streambroker.NewMemoryStreamBroker()

	bLLM := &scriptedLLM{script: []*responses.Response{
		textResponse("agent-b handling"),                                               // turn 1: specialist answers
		toolCallResponse("call_back", "transfer_to_agent", `{"agent_name":"agent-a"}`), // turn 2: hand back
	}}
	agentB := newScriptedAgent("agent-b", bLLM, hist, broker, nil, nil)

	aLLM := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_transfer", "transfer_to_agent", `{"agent_name":"agent-b"}`), // turn 1: hand off
		textResponse("agent-a back in control"),                                            // turn 2: after hand-back
		textResponse("agent-a turn 3"),                                                     // turn 3: still root
	}}
	agentA := agents.NewAgent(&agents.AgentOptions{
		Name:          "agent-a",
		History:       hist,
		StreamBroker:  broker,
		StickyHandoff: true,
		Handoffs: []*agents.Handoff{
			agents.NewHandoff("agent-b", "handles part b", agentB),
		},
	}).WithLLM(aLLM)

	// Wire the reverse edge (specialist → root).
	agentB.AddHandoffs(agents.NewHandoff("agent-a", "back to triage", agentA))

	// Turn 1: root → specialist. Thread becomes sticky to the specialist.
	out := runAgent(t, agentA, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-back",
		Message: userMessage("transfer me"),
	})
	requireStatus(t, out, agentstate.RunStatusCompleted)

	// Turn 2: sticky enters the specialist, which hands back to the root.
	out = runAgent(t, agentA, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-back",
		PreviousMessageID: out.RunID,
		Message:           userMessage("i'm done, go back"),
	})
	requireStatus(t, out, agentstate.RunStatusCompleted)
	if text := messagesText(out.Output); !strings.Contains(text, "agent-a back in control") {
		t.Fatalf("turn 2 didn't hand back to the root, got:\n%s", text)
	}

	// Turn 3: the thread is un-stuck — it re-enters the root directly,
	// without touching the specialist again.
	out = runAgent(t, agentA, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-back",
		PreviousMessageID: out.RunID,
		Message:           userMessage("hello again"),
	})
	requireStatus(t, out, agentstate.RunStatusCompleted)
	if text := messagesText(out.Output); !strings.Contains(text, "agent-a turn 3") {
		t.Fatalf("turn 3 didn't re-enter the root, got:\n%s", text)
	}

	if aLLM.callCount() != 3 {
		t.Fatalf("root LLM called %d times, want 3", aLLM.callCount())
	}
	if bLLM.callCount() != 2 {
		t.Fatalf("specialist LLM called %d times, want 2 (turn 3 must skip it)", bLLM.callCount())
	}
}

func TestAgentLoop_NestedHandoffToolApprovalPausesRun(t *testing.T) {
	persistence := history.NewInMemoryConversationPersistence()
	hist := history.NewConversationManager(persistence)
	broker := streambroker.NewMemoryStreamBroker()

	cLLM := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_c_danger", "c_danger", "{}"),
		textResponse("agent-c finished"),
	}}
	cDanger := newFakeTool("c_danger", true, "c danger done")
	agentC := newScriptedAgent("agent-c", cLLM, hist, broker, []agents.Tool{cDanger}, nil)

	bLLM := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_transfer_c", "transfer_to_agent", `{"agent_name":"agent-c"}`),
	}}
	agentB := newScriptedAgent("agent-b", bLLM, hist, broker, nil, []*agents.Handoff{
		agents.NewHandoff("agent-c", "handles part c", agentC),
	})

	aLLM := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_transfer_b", "transfer_to_agent", `{"agent_name":"agent-b"}`),
	}}
	agentA := newScriptedAgent("agent-a", aLLM, hist, broker, nil, []*agents.Handoff{
		agents.NewHandoff("agent-b", "handles part b", agentB),
	})

	out := runAgent(t, agentA, &agents.AgentInput{
		Namespace: "test",
		ThreadID:  "thread-nested-handoff",
		Message:   userMessage("transfer me twice"),
	})

	// The pause originates two handoffs deep and must surface from the
	// top-level Execute.
	requireStatus(t, out, agentstate.RunStatusPaused)
	requireSinglePendingApproval(t, out, "c_danger", "call_c_danger")
	if cDanger.callCount() != 0 {
		t.Fatalf("nested handoff tool executed %d times before approval, want 0", cDanger.callCount())
	}

	saved, err := persistence.LoadMessages(context.Background(), "test", "thread-nested-handoff", out.RunID)
	if err != nil || len(saved) == 0 {
		t.Fatalf("failed to load persisted run: %v", err)
	}
	runState := agentstate.LoadRunStateFromMeta(saved[len(saved)-1].Meta)
	if runState == nil || runState.LastAgentName != "agent-c" {
		t.Fatalf("persisted last agent = %+v, want agent-c", runState)
	}

	out = runAgent(t, agentC, &agents.AgentInput{
		Namespace:         "test",
		ThreadID:          "thread-nested-handoff",
		PreviousMessageID: out.RunID,
		Message:           approvalMessage([]string{"call_c_danger"}, nil),
	})

	requireStatus(t, out, agentstate.RunStatusCompleted)
	if cDanger.callCount() != 1 {
		t.Fatalf("nested handoff tool executed %d times after approval, want 1", cDanger.callCount())
	}
	if aLLM.callCount() != 1 || bLLM.callCount() != 1 {
		t.Fatalf("upstream LLM calls = a:%d b:%d, want 1 each", aLLM.callCount(), bLLM.callCount())
	}
	if cLLM.callCount() != 2 {
		t.Fatalf("agent-c LLM called %d times, want 2", cLLM.callCount())
	}
	if text := messagesText(out.Output); !strings.Contains(text, "agent-c finished") {
		t.Fatalf("output missing nested handoff answer, got:\n%s", text)
	}
}

// Single-turn ends the run as soon as the model responds. The emitted tool call
// must survive into the output and the tool must NOT execute: offline
// single-turn evals score the model's decision, so executing it (or faking a
// result) would replace the thing under test with an improvised continuation.
func TestAgentLoop_SingleTurn_ReturnsToolCallWithoutExecuting(t *testing.T) {
	// Only one response is scripted: a second model call would exhaust the
	// scripted LLM and fail the run.
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_search", "search", `{"q":"weather"}`),
	}}
	search := newFakeTool("search", false, "search done")
	agent := agents.NewAgent(&agents.AgentOptions{
		Name:       "main",
		Tools:      []agents.Tool{search},
		SingleTurn: true,
	}).WithLLM(llm)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test",
		ThreadID:  "thread-single-turn-tool",
		Message:   userMessage("what's the weather?"),
	})

	requireStatus(t, out, agentstate.RunStatusCompleted)
	if search.callCount() != 0 {
		t.Fatalf("single turn executed the tool %d times, want 0", search.callCount())
	}
	if llm.callCount() != 1 {
		t.Fatalf("single turn made %d model calls, want 1", llm.callCount())
	}

	var emitted bool
	for _, m := range out.Output {
		if m.OfFunctionCall != nil && m.OfFunctionCall.Name == "search" {
			emitted = true
		}
	}
	if !emitted {
		t.Fatalf("single turn dropped the emitted tool call, got: %#v", out.Output)
	}
}

// The common single-turn case: the model answers in text and the run completes
// normally, exactly as it would without the flag.
func TestAgentLoop_SingleTurn_ReturnsText(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{textResponse("sunny")}}
	agent := agents.NewAgent(&agents.AgentOptions{
		Name:       "main",
		SingleTurn: true,
	}).WithLLM(llm)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test",
		ThreadID:  "thread-single-turn-text",
		Message:   userMessage("what's the weather?"),
	})

	requireStatus(t, out, agentstate.RunStatusCompleted)
	if llm.callCount() != 1 {
		t.Fatalf("single turn made %d model calls, want 1", llm.callCount())
	}
	if text := messagesText(out.Output); !strings.Contains(text, "sunny") {
		t.Fatalf("single turn output missing the model's text, got:\n%s", text)
	}
}
