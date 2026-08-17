package summariser

import (
	"context"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"

	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

func msg(id string) messages.Message {
	return messages.Message{
		ID: id,
		Messages: []responses.InputMessageUnion{{
			OfInputMessage: &responses.InputMessage{
				Role:    constants.RoleUser,
				Content: responses.InputContent{{OfInputText: &responses.InputTextContent{Text: id}}},
			},
		}},
	}
}

func runShape(runs []Run) []string {
	out := make([]string, 0, len(runs))
	for _, r := range runs {
		ids := ""
		for _, m := range r.Messages {
			ids += m.ID
		}
		out = append(out, r.RunID+":"+ids)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestGroupIntoRuns(t *testing.T) {
	tests := []struct {
		name  string
		ids   []string
		runOf map[string]string
		want  []string
	}{
		{
			name:  "contiguous runs group together",
			ids:   []string{"a1", "a2", "b1", "b2"},
			runOf: map[string]string{"a1": "A", "a2": "A", "b1": "B", "b2": "B"},
			want:  []string{"A:a1a2", "B:b1b2"},
		},
		{
			// The old rule asked whether "A" had been seen *anywhere* before;
			// it had, so a3 was appended to whatever group was last — B — and
			// the trimming boundary then cut in the wrong place.
			name:  "a run id reappearing out of order starts a new run",
			ids:   []string{"a1", "b1", "a3"},
			runOf: map[string]string{"a1": "A", "b1": "B", "a3": "A"},
			want:  []string{"A:a1", "B:b1", "A:a3"},
		},
		{
			// Unmapped ids resolve to "". They used to pool into a single group
			// regardless of position, so a summary at the head of the list and
			// an unplaced message at the tail became one run spanning the whole
			// conversation.
			name:  "unplaced messages do not pool across placed ones",
			ids:   []string{"s1", "a1", "s2"},
			runOf: map[string]string{"a1": "A"},
			want:  []string{":s1", "A:a1", ":s2"},
		},
		{
			name:  "adjacent unplaced messages group together",
			ids:   []string{"s1", "s2", "a1"},
			runOf: map[string]string{"a1": "A"},
			want:  []string{":s1s2", "A:a1"},
		},
		{
			name:  "empty input yields no runs",
			ids:   nil,
			runOf: map[string]string{},
			want:  []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgs := make([]messages.Message, 0, len(tt.ids))
			for _, id := range tt.ids {
				msgs = append(msgs, msg(id))
			}

			got := runShape(groupIntoRuns(tt.runOf, msgs))
			if !equal(got, tt.want) {
				t.Fatalf("groupIntoRuns = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSlidingWindowUsesContiguousGrouping confirms the shared helper is what
// both summarizers now use — the bug was duplicated because the loop was
// copy-pasted between them.
func TestSlidingWindowUsesContiguousGrouping(t *testing.T) {
	sw := NewSlidingWindowHistorySummarizer(&SlidingWindowHistorySummarizerOptions{KeepCount: 1})

	a1, b1, a3 := msg("a1"), msg("b1"), msg("a3")
	runOf := map[string]string{"a1": "A", "b1": "B", "a3": "A"}

	result, err := sw.Summarize(t.Context(), runOf, []messages.Message{a1, b1, a3}, 0)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if result == nil {
		t.Fatal("Summarize returned nil; three runs with keepCount 1 should trim")
	}

	// Three contiguous runs, keep the last: only a3 survives. Under the old
	// rule a3 was filed into run B, leaving two runs and keeping b1+a3.
	if len(result.MessagesToKeep) != 1 || result.MessagesToKeep[0].ID != "a3" {
		t.Fatalf("MessagesToKeep = %+v, want just a3", result.MessagesToKeep)
	}
}

// fakeProvider answers NewResponses with a canned reply. The remaining Provider
// methods are inherited from the nil embedded interface and panic if the
// summarizer ever reaches for one, which is the assertion we want.
type fakeProvider struct {
	llm.Provider
	resp *responses.Response
}

func (f *fakeProvider) NewResponses(ctx context.Context, in *responses.Request) (*responses.Response, error) {
	return f.resp, nil
}

type fakePrompt string

func (f fakePrompt) GetPrompt(ctx context.Context, _ *agents.Dependencies) (string, error) {
	return string(f), nil
}

// TestSummaryBundleCarriesID pins the second half of the grouping defect,
// through the real construction path. The summary was built as a bare
// messages.Message, so its bundle id was empty — and LoadMessages keys the run
// map by bundle id, so on reload the summary registered under "" and every
// other unplaced message resolved to the summary's run. Its SenderID must stay
// empty so attributeMessages passes it through rather than rewriting it as
// "(Agent) … said:".
func TestSummaryBundleCarriesID(t *testing.T) {
	llmUsage := &responses.Usage{InputTokens: 5000, OutputTokens: 200, TotalTokens: 5200}
	s := NewLLMHistorySummarizer(&LLMHistorySummarizerOptions{
		LLM: &fakeProvider{resp: &responses.Response{
			Output: []responses.OutputMessageUnion{{
				OfOutputMessage: &responses.OutputMessage{
					ID:   "msg-1",
					Role: constants.RoleAssistant,
					Content: &responses.OutputContent{
						{OfOutputText: &responses.OutputTextContent{Text: "a summary"}},
					},
				},
			}},
			Usage: llmUsage,
		}},
		Instruction:     fakePrompt("summarize this"),
		TokenThreshold:  1000,
		KeepRecentCount: 2,
	})

	msgs := []messages.Message{msg("a"), msg("b"), msg("c"), msg("d")}
	runOf := map[string]string{"a": "A", "b": "B", "c": "C", "d": "D"}

	result, err := s.Summarize(t.Context(), runOf, msgs, 50000)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if result == nil || result.Summary == nil {
		t.Fatal("expected a summary; four runs over threshold with keepRecentCount 2")
	}

	if result.Summary.ID == "" {
		t.Error("summary bundle has no id; it will register under \"\" in the run map")
	}
	if result.Summary.SenderID != "" {
		t.Errorf("summary SenderID = %q, want empty so attribution leaves it alone", result.Summary.SenderID)
	}

	// The summarizer also reports what the call cost, so the run manager can
	// bill it without disturbing the context signal.
	if result.Usage == nil || result.Usage.TotalTokens != 5200 {
		t.Errorf("Usage = %+v, want the provider's reported 5200 total", result.Usage)
	}
}
