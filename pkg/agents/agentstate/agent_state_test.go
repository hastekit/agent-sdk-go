package agentstate

import (
	"testing"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// TestTransitionToAwaitApproval_PersistsInterrupts pins the fix for
// tool-level RequiresApproval pauses: the synthesized approval interrupt is
// written into RunState.Interrupts (so it persists via ToMeta), not just
// computed on the wire by PendingInterrupts.
func TestTransitionToAwaitApproval_PersistsInterrupts(t *testing.T) {
	s := NewRunState()
	s.TransitionToAwaitApproval([]responses.FunctionCallMessage{
		{CallID: "call_danger", Name: "dangerous"},
	})

	intr, ok := s.Interrupts["call_danger"]
	if !ok {
		t.Fatalf("interrupt not persisted in RunState.Interrupts: %+v", s.Interrupts)
	}
	if intr.Mode != responses.InterruptModeApproval {
		t.Fatalf("interrupt mode = %q, want approval", intr.Mode)
	}

	// And it survives the ToMeta -> JSON -> Load round-trip.
	buf, _ := sonic.Marshal(s.ToMeta())
	var meta map[string]any
	if err := sonic.Unmarshal(buf, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	loaded := LoadRunStateFromMeta(meta)
	if _, ok := loaded.Interrupts["call_danger"]; !ok {
		t.Fatalf("interrupt lost across persistence: %+v", loaded.Interrupts)
	}
}

// TestToMeta_EmitsPendingInterruptsForReload pins the reload fix: a paused
// run persists pending_interrupts (mode + payload), matching the streaming
// pause chunk, so a reloaded conversation renders a URL elicitation as a
// connect card rather than falling back to the approval widget.
func TestToMeta_EmitsPendingInterruptsForReload(t *testing.T) {
	s := NewRunState()
	urlCall := responses.FunctionCallMessage{CallID: "call_url", Name: "search"}
	s.Interrupts = map[string]responses.Interrupt{
		"call_url": {FunctionCallMessage: urlCall, Mode: responses.InterruptModeURL},
	}
	s.PendingToolCalls = []responses.FunctionCallMessage{urlCall}
	s.CurrentStep = StepAwaitApproval

	buf, _ := sonic.Marshal(s.ToMeta())
	var meta map[string]any
	if err := sonic.Unmarshal(buf, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}

	runState, ok := meta["run_state"].(map[string]any)
	if !ok {
		t.Fatalf("no run_state in meta: %+v", meta)
	}
	pi, ok := runState["pending_interrupts"].([]any)
	if !ok || len(pi) != 1 {
		t.Fatalf("pending_interrupts not persisted: %+v", runState["pending_interrupts"])
	}
	first := pi[0].(map[string]any)
	if first["mode"] != string(responses.InterruptModeURL) {
		t.Fatalf("persisted interrupt mode = %v, want url", first["mode"])
	}
}

// TestToMeta_StoresFunctionCallOnce pins the persistence dedup: a paused run
// stores each pending call only in pending_interrupts (not also in
// pending_tool_calls and the interrupts map), and LoadRunStateFromMeta
// reconstructs both PendingToolCalls and Interrupts from that single copy.
func TestToMeta_StoresFunctionCallOnce(t *testing.T) {
	s := NewRunState()
	s.TransitionToAwaitApproval([]responses.FunctionCallMessage{
		{CallID: "call_1", Name: "messenger-agent", Arguments: `{"message":"hi"}`},
	})

	buf, _ := sonic.Marshal(s.ToMeta())
	var meta map[string]any
	if err := sonic.Unmarshal(buf, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	runState := meta["run_state"].(map[string]any)

	// Only pending_interrupts holds the call — the redundant copies are gone.
	if _, ok := runState["pending_interrupts"]; !ok {
		t.Fatalf("pending_interrupts missing: %+v", runState)
	}
	if _, ok := runState["pending_tool_calls"]; ok {
		t.Fatalf("pending_tool_calls should no longer be persisted: %+v", runState)
	}
	if _, ok := runState["interrupts"]; ok {
		t.Fatalf("interrupts map should no longer be persisted: %+v", runState)
	}

	// Both in-memory fields are reconstructed from the single copy on load.
	loaded := LoadRunStateFromMeta(meta)
	if len(loaded.PendingToolCalls) != 1 || loaded.PendingToolCalls[0].CallID != "call_1" {
		t.Fatalf("PendingToolCalls not reconstructed: %+v", loaded.PendingToolCalls)
	}
	intr, ok := loaded.Interrupts["call_1"]
	if !ok || intr.Mode != responses.InterruptModeApproval || intr.FunctionCallMessage.Name != "messenger-agent" {
		t.Fatalf("Interrupts not reconstructed: %+v", loaded.Interrupts)
	}
}

// TestContextTokensRoundTrip ensures the non-cumulative ContextTokens signal
// (used by the summarizer to gauge current context size) survives the
// ToMeta -> JSON -> LoadRunStateFromMeta persistence round-trip. The JSON hop
// mirrors how meta is actually stored (numbers come back as float64).
func TestContextTokensRoundTrip(t *testing.T) {
	in := NewRunState()
	in.CurrentStep = StepComplete
	in.ContextTokens = 12345

	// Round-trip through JSON the same way meta is persisted/loaded.
	raw, err := sonic.Marshal(in.ToMeta())
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	var meta map[string]any
	if err := sonic.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}

	out := LoadRunStateFromMeta(meta)
	if out == nil {
		t.Fatal("LoadRunStateFromMeta returned nil")
	}
	if out.ContextTokens != 12345 {
		t.Fatalf("ContextTokens not preserved: got %d, want 12345", out.ContextTokens)
	}
}

// TestRunStateRoundTripWithoutJSONHop covers the other persistence shape: an
// adapter that keeps the meta map in process (the in-memory and file adapters
// within a process lifetime) hands ToMeta's values straight back, so numbers
// are Go ints and usage is still a responses.Usage struct — not the float64s
// and maps a JSON hop produces. The loader must read both.
func TestRunStateRoundTripWithoutJSONHop(t *testing.T) {
	in := NewRunState()
	in.CurrentStep = StepComplete
	in.ContextTokens = 12345
	in.LoopIteration = 7
	in.Usage = responses.Usage{InputTokens: 900, OutputTokens: 100, TotalTokens: 1000}

	out := LoadRunStateFromMeta(in.ToMeta())
	if out == nil {
		t.Fatal("LoadRunStateFromMeta returned nil")
	}
	if out.ContextTokens != 12345 {
		t.Errorf("ContextTokens = %d, want 12345", out.ContextTokens)
	}
	if out.LoopIteration != 7 {
		t.Errorf("LoopIteration = %d, want 7", out.LoopIteration)
	}
	if out.Usage.TotalTokens != 1000 || out.Usage.InputTokens != 900 {
		t.Errorf("Usage = %+v, want input=900 total=1000", out.Usage)
	}
}
