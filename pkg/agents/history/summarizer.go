package history

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// SummaryResult contains the result of summarization including metadata needed for saving
type SummaryResult struct {
	Summary             *messages.Message // The summary message
	MessagesToKeep      []messages.Message
	LastSummarizedRunID string // ID of the last run that was summarized
	SummaryID           string // Unique ID for the summary (generated if empty)

	// Usage reports what producing this summary cost, for summarizers that
	// call a model to do it. The run manager bills it to the run without
	// letting it disturb the context-occupancy signal — see
	// ConversationRunManager.TrackAuxiliaryUsage. Nil for summarizers that
	// need no model call, such as the sliding window.
	//
	// It rides back on the result rather than being reported by the summarizer
	// directly because that is the value the durable runtimes already serialize
	// across the activity boundary; a summarizer running as a Temporal or
	// Restate activity has no handle on the run manager to report to.
	Usage *responses.Usage
}

type HistorySummarizer interface {
	// ShouldSummarize is a deterministic, side-effect-free decision based on the
	// supplied history, current context size, and summarizer configuration.
	// It runs locally, including during durable workflow replay: no I/O,
	// model calls, wall-clock reads, or mutable external state.
	ShouldSummarize(ctx context.Context, msgIdToRunId map[string]string, messages []messages.Message, contextTokens int) (bool, error)
	// Summarize takes a list of messages and returns a summary result (nil when skipped).
	Summarize(ctx context.Context, msgIdToRunId map[string]string, messages []messages.Message, contextTokens int) (*SummaryResult, error)
}

// SummarizationObserver receives start and end notifications around a summarizer
// execution after its decision to compact. End includes the result or error. Observers
// run in the caller, outside any durable summarizer activity.
type SummarizationObserver func(started bool, result *SummaryResult, err error)
