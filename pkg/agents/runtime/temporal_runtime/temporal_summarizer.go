package temporal_runtime

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"go.temporal.io/sdk/workflow"
)

type TemporalConversationSummarizer struct {
	wrappedSummarizer history.HistorySummarizer
}

func NewTemporalConversationSummarizer(wrappedSummarizer history.HistorySummarizer) *TemporalConversationSummarizer {
	return &TemporalConversationSummarizer{wrappedSummarizer: wrappedSummarizer}
}

func (t *TemporalConversationSummarizer) Summarize(ctx context.Context, msgIdToRunId map[string]string, msgs []messages.Message, contextTokens int) (*history.SummaryResult, error) {
	return t.wrappedSummarizer.Summarize(ctx, msgIdToRunId, msgs, contextTokens)
}

type TemporalConversationSummarizerProxy struct {
	workflowCtx       workflow.Context
	prefix            string
	ref               *agents.AgentRef
	wrappedSummarizer history.HistorySummarizer

	// decide is the local activity that decides for a factory agent, whose summarizer lives on the worker.
	decide any
}

func NewTemporalConversationSummarizerProxy(workflowCtx workflow.Context, prefix string, summarizer history.HistorySummarizer) history.HistorySummarizer {
	return &TemporalConversationSummarizerProxy{
		workflowCtx:       workflowCtx,
		prefix:            prefix,
		wrappedSummarizer: summarizer,
	}
}

func (t *TemporalConversationSummarizerProxy) Summarize(ctx context.Context, msgIdToRunId map[string]string, msgs []messages.Message, contextTokens int) (*history.SummaryResult, error) {
	var summaryResult *history.SummaryResult
	err := executeActivity(t.workflowCtx, t.ref, t.prefix+"_SummarizerActivity", msgIdToRunId, msgs, contextTokens).Get(t.workflowCtx, &summaryResult)
	if err != nil {
		return nil, err
	}

	return summaryResult, nil
}

func (t *TemporalConversationSummarizerProxy) ShouldSummarize(ctx context.Context, ids map[string]string, msgs []messages.Message, tokens int) (bool, error) {
	if t.decide != nil && t.ref != nil {
		// A local activity journals only the decision, not the messages it was shown.
		return shouldSummarizeLocally(t.workflowCtx, t.decide, *t.ref, ids, msgs, tokens)
	}
	return t.wrappedSummarizer.ShouldSummarize(ctx, ids, msgs, tokens)
}
