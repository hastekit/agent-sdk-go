package history

import (
	"context"
	"strings"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

func toolResult(callID, output string) Message {
	return messages.New("agent", []responses.InputMessageUnion{{
		OfFunctionCallOutput: &responses.FunctionCallOutputMessage{
			CallID: callID,
			Output: responses.FunctionCallOutputContentUnion{OfString: &output},
		},
	}})
}

// TestEstimateBundleTokens checks the estimator tracks content size across the
// message shapes a run actually produces.
func TestEstimateBundleTokens(t *testing.T) {
	// 400 characters of tool output ≈ 100 tokens, plus per-message overhead.
	big := toolResult("call-1", strings.Repeat("x", 400))
	if got := estimateBundleTokens(big); got != 104 {
		t.Errorf("tool result estimate = %d, want 104 (400/4 + overhead)", got)
	}

	// A function call counts its name and its serialized arguments — the
	// arguments are usually the bulk of it.
	call := messages.New("agent", []responses.InputMessageUnion{{
		OfFunctionCall: &responses.FunctionCallMessage{
			CallID:    "call-1",
			Name:      "search",
			Arguments: strings.Repeat("a", 200),
		},
	}})
	if got := estimateBundleTokens(call); got < 50 {
		t.Errorf("function call estimate = %d, want the arguments to dominate", got)
	}

	// Empty bundles still cost the framing, never a negative or wild number.
	if got := estimateBundleTokens(messages.New("agent", nil)); got != 0 {
		t.Errorf("empty bundle estimate = %d, want 0", got)
	}
}

// TestEstimateIgnoresImagePayloadSize is the case a naive character count gets
// badly wrong. Images are billed by dimension, so measuring a base64 data URL
// would turn one screenshot into a six-figure token estimate and trigger
// summarization on every call.
func TestEstimateIgnoresImagePayloadSize(t *testing.T) {
	dataURL := "data:image/png;base64," + strings.Repeat("A", 400_000)
	img := messages.New("user", []responses.InputMessageUnion{{
		OfInputMessage: &responses.InputMessage{
			Role: constants.RoleUser,
			Content: responses.InputContent{
				{OfInputImage: &responses.InputImageContent{ImageURL: &dataURL}},
			},
		},
	}})

	got := estimateBundleTokens(img)
	if got > 5000 {
		t.Fatalf("image estimate = %d; the base64 payload is being counted as text", got)
	}
	if got < imageTokens {
		t.Fatalf("image estimate = %d, want at least the nominal %d", got, imageTokens)
	}
}

// TestPendingEstimateReplacedByMeasurement is the handoff the whole design
// rests on: messages appended after a call are estimated, and the estimate is
// discarded as soon as the next call reports what the prompt really was.
func TestPendingEstimateReplacedByMeasurement(t *testing.T) {
	ctx := context.Background()
	p := NewInMemoryConversationPersistence()
	cm := NewConversationManager(p)

	run, err := NewRun(ctx, cm, "ns", "thread-1", "")
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	// Nothing measured yet; the user's turn is estimated.
	run.AddMessages(ctx, userTurn("hello"))
	if run.RunState.PendingContextTokens == 0 {
		t.Fatal("first turn was not estimated")
	}
	if run.RunState.ContextTokens != 0 {
		t.Fatalf("ContextTokens = %d, want 0 before any call", run.RunState.ContextTokens)
	}

	// The call reports real numbers for both the prompt and the reply,
	// superseding the estimate entirely.
	run.TrackUsage(&responses.Usage{InputTokens: 12000, OutputTokens: 800, TotalTokens: 12800})
	if got := run.RunState.PendingContextTokens; got != 0 {
		t.Fatalf("PendingContextTokens = %d, want 0 — nothing is unmeasured yet", got)
	}
	if got := run.contextTokens(); got != 12800 {
		t.Fatalf("contextTokens() = %d, want 12800 (prompt + reply, both measured)", got)
	}

	// Appending that reply must not count it a second time.
	run.AddMessages(ctx, assistantTurn(strings.Repeat("z", 3000)), AlreadyMeasured())
	if got := run.contextTokens(); got != 12800 {
		t.Fatalf("contextTokens() = %d after appending the reply, want 12800 — it was already counted", got)
	}

	// A large tool result lands after the call. Nothing has measured it, so the
	// estimate must carry it.
	run.AddMessages(ctx, toolResult("call-1", strings.Repeat("x", 40_000)))

	got := run.contextTokens()
	if got <= 12800 {
		t.Fatalf("contextTokens() = %d, want > 12800 — the tool result is unmeasured", got)
	}
	if got < 20000 {
		t.Fatalf("contextTokens() = %d; a 40k-character tool result should add ~10k tokens", got)
	}

	// The next call measures the whole thing. The estimate is dropped rather
	// than compounding on top of the new measurement.
	run.TrackUsage(&responses.Usage{InputTokens: 22500, OutputTokens: 300, TotalTokens: 22800})
	if got := run.contextTokens(); got != 22800 {
		t.Fatalf("contextTokens() = %d, want 22800 (22500 prompt + 300 reply)", got)
	}
}

// TestPendingEstimateSurvivesTurnBoundary covers the carry-forward, in the one
// shape that actually needs it. A run that ends normally does so right after an
// LLM call, so its usage report has just cleared the estimate and zero is what
// crosses the boundary. A *cancelled* run is different: it appends synthetic
// tool results and a cancellation notice, then completes without calling the
// model again, so those messages reach the next turn unmeasured. This models
// that shape — unmeasured messages appended, then straight to complete.
func TestPendingEstimateSurvivesTurnBoundary(t *testing.T) {
	ctx := context.Background()
	p := NewInMemoryConversationPersistence()
	cm := NewConversationManager(p)

	run1, err := NewRun(ctx, cm, "ns", "thread-1", "")
	if err != nil {
		t.Fatalf("NewRun (turn 1): %v", err)
	}
	run1.AddMessages(ctx, userTurn("hello"))
	run1.TrackUsage(&responses.Usage{InputTokens: 5000, OutputTokens: 400, TotalTokens: 5400})

	// The reply is appended after the measurement, exactly as the agent loop
	// does it. It is already inside the reported total, so it adds no estimate.
	run1.AddMessages(ctx, assistantTurn(strings.Repeat("y", 2000)), AlreadyMeasured())
	if got := run1.RunState.PendingContextTokens; got != 0 {
		t.Fatalf("PendingContextTokens = %d, want 0 — the reply was already measured", got)
	}

	// A tool result, though, is unmeasured and must be estimated.
	run1.AddMessages(ctx, toolResult("call-1", strings.Repeat("t", 1200)))
	pending := run1.RunState.PendingContextTokens
	if pending == 0 {
		t.Fatal("the tool result was not estimated")
	}

	run1.RunState.TransitionToComplete()
	if err := run1.SaveMessages(ctx); err != nil {
		t.Fatalf("SaveMessages: %v", err)
	}

	run2, err := NewRun(ctx, cm, "ns", "thread-1", "")
	if err != nil {
		t.Fatalf("NewRun (turn 2): %v", err)
	}

	if got := run2.RunState.PendingContextTokens; got != pending {
		t.Fatalf("PendingContextTokens across the boundary = %d, want %d", got, pending)
	}
	if got := run2.contextTokens(); got != 5400+pending {
		t.Fatalf("contextTokens() = %d, want %d", got, 5400+pending)
	}
}
