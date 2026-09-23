package runtime_test

import (
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history/summariser"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/agents/prompts"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/restate_runtime"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/temporal_runtime"
	"github.com/stretchr/testify/require"
)

func TestSummarizerDecisionNeedsNoDurableContext(t *testing.T) {
	local := summariser.NewLLMHistorySummarizer(&summariser.LLMHistorySummarizerOptions{
		Instruction: prompts.New("Summarize"), TokenThreshold: 100, KeepRecentCount: 1,
	})
	// Nil workflow contexts and no LLM ensure the decision performs no runtime
	// operation or model call. It uses only configuration and supplied state.
	for name, proxy := range map[string]history.HistorySummarizer{
		"temporal": temporal_runtime.NewTemporalConversationSummarizerProxy(nil, "agent", local),
		"restate":  restate_runtime.NewRestateConversationSummarizer(nil, local),
	} {
		t.Run(name, func(t *testing.T) {
			ids := map[string]string{"a": "run-a", "b": "run-b"}
			msgs := []messages.Message{{ID: "a"}, {ID: "b"}}
			for _, tokens := range []int{99, 100, 99, 100} {
				decision, err := proxy.ShouldSummarize(t.Context(), ids, msgs, tokens)
				require.NoError(t, err)
				require.Equal(t, tokens >= 100, decision)
			}
		})
	}
	manager := history.NewConversationManager(history.NewInMemoryConversationPersistence(), history.WithSummarizer(local))
	activities := temporal_runtime.NewTemporalAgent(nil, &agents.AgentOptions{Name: "agent", History: manager}, nil).GetActivities()
	require.Contains(t, activities, "agent_SummarizerActivity")
	require.NotContains(t, activities, "agent_ShouldSummarizeActivity")
}
