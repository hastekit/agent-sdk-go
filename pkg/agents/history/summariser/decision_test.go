package summariser

import (
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/agents/prompts"
	"github.com/stretchr/testify/require"
)

func TestSummarizationDecision(t *testing.T) {
	ctx := t.Context()
	ids := map[string]string{"a": "first", "b": "second"}
	msgs := []messages.Message{{ID: "a"}, {ID: "b"}}
	llm := NewLLMHistorySummarizer(&LLMHistorySummarizerOptions{
		TokenThreshold: 100, KeepRecentCount: 1, Instruction: prompts.New("Summarize"),
	})
	for _, tc := range []struct {
		tokens int
		msgs   []messages.Message
		want   bool
	}{{99, msgs, false}, {100, msgs, true}, {101, msgs[:1], false}} {
		decision, err := llm.ShouldSummarize(ctx, ids, tc.msgs, tc.tokens)
		require.NoError(t, err)
		require.Equal(t, tc.want, decision)
	}
	llm.instruction = nil
	decision, err := llm.ShouldSummarize(ctx, ids, msgs, 100)
	require.NoError(t, err)
	require.False(t, decision)

	window := NewSlidingWindowHistorySummarizer(&SlidingWindowHistorySummarizerOptions{KeepCount: 1})
	decision, err = window.ShouldSummarize(ctx, ids, msgs[:1], 0)
	require.NoError(t, err)
	require.False(t, decision)
	decision, err = window.ShouldSummarize(ctx, ids, msgs, 0)
	require.NoError(t, err)
	require.True(t, decision)
}
