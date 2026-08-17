package history

import (
	"context"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// recordingSummarizer captures the contextTokens the run manager hands the
// summarizer, without producing a summary.
type recordingSummarizer struct {
	seen []int
}

func (r *recordingSummarizer) Summarize(ctx context.Context, msgIdToRunId map[string]string, msgs []Message, contextTokens int) (*SummaryResult, error) {
	r.seen = append(r.seen, contextTokens)
	return nil, nil
}

func userTurn(text string) Message {
	return messages.New("user", []responses.InputMessageUnion{{
		OfInputMessage: &responses.InputMessage{
			Role:    constants.RoleUser,
			Content: responses.InputContent{{OfInputText: &responses.InputTextContent{Text: text}}},
		},
	}})
}

// TestNewRun_CarriesContextTokensAcrossTurns pins the carry-forward of
// ContextTokens over a turn boundary. It measures how full the context window
// is, which is a property of the thread rather than of a single run — zeroing
// it made the first GetMessages of every turn report "no context yet".
func TestNewRun_CarriesContextTokensAcrossTurns(t *testing.T) {
	ctx := context.Background()
	p := NewInMemoryConversationPersistence()
	cm := NewConversationManager(p)

	run1, err := NewRun(ctx, cm, "ns", "thread-1", "")
	if err != nil {
		t.Fatalf("NewRun (turn 1): %v", err)
	}
	run1.AddMessages(ctx, userTurn("hello"))
	run1.TrackUsage(&responses.Usage{InputTokens: 40000, OutputTokens: 2000, TotalTokens: 42000})
	run1.RunState.TransitionToComplete()
	if err := run1.SaveMessages(ctx); err != nil {
		t.Fatalf("SaveMessages (turn 1): %v", err)
	}

	run2, err := NewRun(ctx, cm, "ns", "thread-1", "")
	if err != nil {
		t.Fatalf("NewRun (turn 2): %v", err)
	}

	if got := run2.RunState.ContextTokens; got != 42000 {
		t.Fatalf("ContextTokens after turn boundary = %d, want 42000", got)
	}

	// Everything else about the run state is still fresh: only context
	// occupancy (and the sticky agent name) survives the boundary.
	if got := run2.RunState.LoopIteration; got != 0 {
		t.Errorf("LoopIteration = %d, want 0", got)
	}
	if got := run2.RunState.Usage.TotalTokens; got != 0 {
		t.Errorf("Usage.TotalTokens = %d, want 0 (per-run accumulator)", got)
	}
	if got := run2.RunState.CurrentStep; got != agentstate.StepCallLLM {
		t.Errorf("CurrentStep = %q, want %q", got, agentstate.StepCallLLM)
	}
}

// TestGetMessages_FirstCallOfTurnSeesContextTokens covers the behaviour the
// carry-forward exists for: the summarizer runs before the turn's first LLM
// call, so an agent that answers in a single call per turn — never entering a
// tool loop — must still see a non-zero token count, or it would never
// summarize at all.
func TestGetMessages_FirstCallOfTurnSeesContextTokens(t *testing.T) {
	ctx := context.Background()
	p := NewInMemoryConversationPersistence()
	rec := &recordingSummarizer{}
	cm := NewConversationManager(p, WithSummarizer(rec))

	run1, err := NewRun(ctx, cm, "ns", "thread-1", "")
	if err != nil {
		t.Fatalf("NewRun (turn 1): %v", err)
	}
	if _, err := run1.GetMessages(ctx, "agent"); err != nil {
		t.Fatalf("GetMessages (turn 1): %v", err)
	}
	run1.AddMessages(ctx, userTurn("hello"))
	run1.TrackUsage(&responses.Usage{InputTokens: 40000, OutputTokens: 2000, TotalTokens: 42000})
	run1.RunState.TransitionToComplete()
	if err := run1.SaveMessages(ctx); err != nil {
		t.Fatalf("SaveMessages (turn 1): %v", err)
	}

	run2, err := NewRun(ctx, cm, "ns", "thread-1", "")
	if err != nil {
		t.Fatalf("NewRun (turn 2): %v", err)
	}
	if _, err := run2.GetMessages(ctx, "agent"); err != nil {
		t.Fatalf("GetMessages (turn 2): %v", err)
	}

	// 42000 = the measured prompt plus the measured reply, both of which are in
	// the next prompt. Nothing here is estimated.
	want := []int{0, 42000}
	if len(rec.seen) != len(want) {
		t.Fatalf("summarizer saw %v, want %v", rec.seen, want)
	}
	for i := range want {
		if rec.seen[i] != want[i] {
			t.Fatalf("summarizer saw %v, want %v", rec.seen, want)
		}
	}
}
