package history

import (
	"context"

	"github.com/hastekit/hastekit-sdk-go/pkg/agents/messages"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
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
	// Summarize takes a list of messages and returns a summary result.
	// If summarization is not needed, returns a result with KeepFromIndex = -1.
	Summarize(ctx context.Context, msgIdToRunId map[string]string, messages []messages.Message, contextTokens int) (*SummaryResult, error)
}
