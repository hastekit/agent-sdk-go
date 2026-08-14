package history_test

import (
	"context"
	"testing"

	"github.com/hastekit/hastekit-sdk-go/pkg/agents/history"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/history/summariser"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/messages"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
)

func turn(text string) history.Message {
	return messages.New("user", []responses.InputMessageUnion{{
		OfInputMessage: &responses.InputMessage{
			Role:    constants.RoleUser,
			Content: responses.InputContent{{OfInputText: &responses.InputTextContent{Text: text}}},
		},
	}})
}

// TestSlidingWindowZeroKeepCountKeepsTheCurrentRun pins the floor on KeepCount.
// The option is a plain int, so the zero value is what you get by not setting
// the field — and taking it verbatim discarded every run, the one in flight
// included, leaving the model with no conversation at all. Its sibling
// NewLLMHistorySummarizer already defaulted; this one did not.
func TestSlidingWindowZeroKeepCountKeepsTheCurrentRun(t *testing.T) {
	ctx := context.Background()

	sw := summariser.NewSlidingWindowHistorySummarizer(&summariser.SlidingWindowHistorySummarizerOptions{})

	older, current := turn("older run"), turn("current run")
	runIDs := map[string]string{older.ID: "run-1", current.ID: "run-2"}

	result, err := sw.Summarize(ctx, runIDs, []history.Message{older, current}, 0)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if result == nil {
		t.Fatal("Summarize returned nil; expected the older run to be dropped")
	}

	if len(result.MessagesToKeep) != 1 || result.MessagesToKeep[0].ID != current.ID {
		t.Fatalf("MessagesToKeep = %+v, want just the current run", result.MessagesToKeep)
	}
	if result.LastSummarizedRunID != "run-1" {
		t.Errorf("LastSummarizedMessageID = %q, want \"run-1\" — the boundary must stop short of the run in flight",
			result.LastSummarizedRunID)
	}
}

// TestSlidingWindowKeepsExplicitCount confirms the floor only fills in an unset
// value and does not override a real one.
func TestSlidingWindowKeepsExplicitCount(t *testing.T) {
	ctx := context.Background()

	sw := summariser.NewSlidingWindowHistorySummarizer(
		&summariser.SlidingWindowHistorySummarizerOptions{KeepCount: 2})

	a, b, c := turn("one"), turn("two"), turn("three")
	runIDs := map[string]string{a.ID: "run-1", b.ID: "run-2", c.ID: "run-3"}

	result, err := sw.Summarize(ctx, runIDs, []history.Message{a, b, c}, 0)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if result == nil {
		t.Fatal("Summarize returned nil; expected the oldest run to be dropped")
	}
	if len(result.MessagesToKeep) != 2 {
		t.Fatalf("MessagesToKeep = %d runs, want 2", len(result.MessagesToKeep))
	}
}
