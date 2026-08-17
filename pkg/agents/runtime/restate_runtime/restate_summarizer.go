package restate_runtime

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	restate "github.com/restatedev/sdk-go"
)

type RestateConversationSummarizer struct {
	restateCtx        restate.WorkflowContext
	wrappedSummarizer history.HistorySummarizer
}

func NewRestateConversationSummarizer(restateCtx restate.WorkflowContext, wrappedSummarizer history.HistorySummarizer) *RestateConversationSummarizer {
	return &RestateConversationSummarizer{
		restateCtx:        restateCtx,
		wrappedSummarizer: wrappedSummarizer,
	}
}

func (t *RestateConversationSummarizer) Summarize(ctx context.Context, msgIdToRunId map[string]string, msgs []messages.Message, contextTokens int) (*history.SummaryResult, error) {
	return restate.Run(t.restateCtx, func(ctx restate.RunContext) (*history.SummaryResult, error) {
		return t.wrappedSummarizer.Summarize(ctx, msgIdToRunId, msgs, contextTokens)
	})
}
