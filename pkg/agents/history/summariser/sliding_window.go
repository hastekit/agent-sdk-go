package summariser

import (
	"context"

	"github.com/google/uuid"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
)

type SlidingWindowHistorySummarizer struct {
	keepCount int // Number of recent runs to keep
}

type SlidingWindowHistorySummarizerOptions struct {
	KeepCount int // Number of recent runs to keep
}

func NewSlidingWindowHistorySummarizer(opts *SlidingWindowHistorySummarizerOptions) *SlidingWindowHistorySummarizer {
	// Floor the window at one run, matching NewLLMHistorySummarizer. Taking
	// KeepCount verbatim made the zero value — what you get by not setting the
	// field — discard every run, including the one in flight, leaving the model
	// with no conversation at all.
	keepCount := 1
	if opts.KeepCount > 0 {
		keepCount = opts.KeepCount
	}

	return &SlidingWindowHistorySummarizer{
		keepCount: keepCount,
	}
}

// Summarize implements the HistorySummarizer interface.
// For sliding window, we simply keep the most recent N runs and discard the rest.
// We don't create a summary message, we just return which messages to keep.
func (s *SlidingWindowHistorySummarizer) Summarize(ctx context.Context, msgIdToRunId map[string]string, msgs []messages.Message, contextTokens int) (*history.SummaryResult, error) {
	runs := groupIntoRuns(msgIdToRunId, msgs)

	// If we have fewer or equal runs than keepCount, keep everything
	if len(runs) <= s.keepCount {
		return nil, nil
	}

	// Keep only the most recent keepCount runs
	keepFromIndex := len(runs) - s.keepCount
	runsToKeep := runs[keepFromIndex:]
	runsToDiscard := runs[:keepFromIndex]

	// Collect all messages from runs to keep
	messagesToKeep := []messages.Message{}
	for _, run := range runsToKeep {
		messagesToKeep = append(messagesToKeep, run.Messages...)
	}

	// Find the last message ID that was discarded (the last run ID before keepFromIndex)
	var lastDiscardedRunID string
	for i := len(runsToDiscard) - 1; i >= 0; i-- {
		if runsToDiscard[i].RunID != "" {
			lastDiscardedRunID = runsToDiscard[i].RunID
			break
		}
	}

	// For sliding window, we don't create a summary message
	// We just return the messages to keep
	// The LastSummarizedMessageID represents the last run ID that was discarded
	return &history.SummaryResult{
		LastSummarizedRunID: lastDiscardedRunID,
		SummaryID:           uuid.NewString(),
		MessagesToKeep:      messagesToKeep,
	}, nil
}
