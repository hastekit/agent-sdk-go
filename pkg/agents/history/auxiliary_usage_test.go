package history

import (
	"context"
	"testing"

	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
)

// usageReportingSummarizer reports a cost for the summary it produces, the way
// a model-backed summarizer does.
type usageReportingSummarizer struct {
	usage         *responses.Usage
	sawContextTok []int
}

func (u *usageReportingSummarizer) Summarize(ctx context.Context, msgIdToRunId map[string]string, msgs []Message, contextTokens int) (*SummaryResult, error) {
	u.sawContextTok = append(u.sawContextTok, contextTokens)
	return &SummaryResult{
		SummaryID:      "sum-1",
		MessagesToKeep: msgs,
		Usage:          u.usage,
	}, nil
}

// TestSummarizerUsageBilledWithoutDisturbingContextTokens is the whole point of
// splitting TrackUsage. A summarization request is its own prompt — instruction
// plus a flattened transcript, no tools, no agent system prompt — so its size is
// not a reading of the agent's context window. Billing it through TrackUsage
// would have overwritten the signal the summarizer triggers on with a
// measurement of a different conversation.
func TestSummarizerUsageBilledWithoutDisturbingContextTokens(t *testing.T) {
	ctx := context.Background()
	p := NewInMemoryConversationPersistence()
	sum := &usageReportingSummarizer{usage: &responses.Usage{
		InputTokens:  8000,
		OutputTokens: 400,
		TotalTokens:  8400,
	}}
	cm := NewConversationManager(p, WithSummarizer(sum))

	run, err := NewRun(ctx, cm, "ns", "thread-1", "")
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	run.AddMessages(ctx, userTurn("hello"))

	// The agent's own call sets the context-occupancy signal.
	run.TrackUsage(&responses.Usage{InputTokens: 120000, OutputTokens: 500, TotalTokens: 120500})

	if _, err := run.GetMessages(ctx, "agent"); err != nil {
		t.Fatalf("GetMessages: %v", err)
	}

	// The summary's cost is billed...
	if got := run.RunState.Usage.TotalTokens; got != 128900 {
		t.Errorf("Usage.TotalTokens = %d, want 128900 (120500 agent + 8400 summarizer)", got)
	}
	if got := run.RunState.Usage.InputTokens; got != 128000 {
		t.Errorf("Usage.InputTokens = %d, want 128000", got)
	}

	// ...but the context signal still describes the agent's conversation:
	// its prompt plus its reply, both measured.
	if got := run.RunState.ContextTokens; got != 120500 {
		t.Fatalf("ContextTokens = %d, want 120500 — the summarization call must not overwrite it", got)
	}
}

// TestAuxiliaryUsageLeavesContextTokensAlone covers the accessor directly,
// including that it is safe on a nil usage (the sliding window reports none).
func TestAuxiliaryUsageLeavesContextTokensAlone(t *testing.T) {
	ctx := context.Background()
	p := NewInMemoryConversationPersistence()
	cm := NewConversationManager(p)

	run, err := NewRun(ctx, cm, "ns", "thread-1", "")
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	run.TrackUsage(&responses.Usage{InputTokens: 1000, OutputTokens: 50, TotalTokens: 1050})
	run.TrackAuxiliaryUsage(&responses.Usage{InputTokens: 300, OutputTokens: 20, TotalTokens: 320})
	run.TrackAuxiliaryUsage(nil)

	if got := run.RunState.ContextTokens; got != 1050 {
		t.Errorf("ContextTokens = %d, want 1050 (the measured prompt plus reply)", got)
	}
	if got := run.RunState.Usage.TotalTokens; got != 1370 {
		t.Errorf("Usage.TotalTokens = %d, want 1370", got)
	}
}

// TestTrackUsageAccumulatesReasoningTokens covers a field the accumulator was
// dropping: reasoning tokens are a subset of OutputTokens, so the run total
// reported them as zero however much thinking the model did.
func TestTrackUsageAccumulatesReasoningTokens(t *testing.T) {
	ctx := context.Background()
	p := NewInMemoryConversationPersistence()
	cm := NewConversationManager(p)

	run, err := NewRun(ctx, cm, "ns", "thread-1", "")
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	first := &responses.Usage{OutputTokens: 900, TotalTokens: 900}
	first.OutputTokensDetails.ReasoningTokens = 700
	second := &responses.Usage{OutputTokens: 300, TotalTokens: 300}
	second.OutputTokensDetails.ReasoningTokens = 200

	run.TrackUsage(first)
	run.TrackUsage(second)

	if got := run.RunState.Usage.OutputTokensDetails.ReasoningTokens; got != 900 {
		t.Fatalf("ReasoningTokens = %d, want 900", got)
	}
}
