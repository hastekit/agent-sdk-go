package temporal_runtime

import (
	"context"

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
	workflowCtx workflow.Context
	prefix      string
}

func NewTemporalConversationSummarizerProxy(workflowCtx workflow.Context, prefix string) history.HistorySummarizer {
	return &TemporalConversationSummarizerProxy{
		workflowCtx: workflowCtx,
		prefix:      prefix,
	}
}

func (t *TemporalConversationSummarizerProxy) Summarize(ctx context.Context, msgIdToRunId map[string]string, msgs []messages.Message, contextTokens int) (*history.SummaryResult, error) {
	var summaryResult *history.SummaryResult
	err := workflow.ExecuteActivity(t.workflowCtx, t.prefix+"_SummarizerActivity", msgIdToRunId, msgs, contextTokens).Get(t.workflowCtx, &summaryResult)
	if err != nil {
		return nil, err
	}

	return summaryResult, nil
}
