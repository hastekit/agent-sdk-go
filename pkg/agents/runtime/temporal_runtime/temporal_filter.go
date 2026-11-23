package temporal_runtime

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"go.temporal.io/sdk/workflow"
)

// TemporalMessageFilter hosts the activity that runs a history.MessageFilter
// on the worker. The filter's transform may hit the DB, so it runs in an
// activity rather than the workflow goroutine.
type TemporalMessageFilter struct {
	wrappedFilter history.MessageFilter
}

func NewTemporalMessageFilter(wrappedFilter history.MessageFilter) *TemporalMessageFilter {
	return &TemporalMessageFilter{wrappedFilter: wrappedFilter}
}

// Filter is the activity entrypoint. It returns (result, error) to satisfy
// the Temporal activity contract; the wrapped MessageFilter can't itself
// error, so the error is always nil.
func (t *TemporalMessageFilter) Filter(ctx context.Context, msgs []messages.Message, agentID string) ([]messages.Message, error) {
	return t.wrappedFilter.Filter(ctx, msgs, agentID), nil
}

// TemporalMessageFilterProxy is the workflow-side MessageFilter. It routes
// the filter through the MessageFilter activity so it always runs as a
// durable, replay-deterministic step.
type TemporalMessageFilterProxy struct {
	workflowCtx workflow.Context
	prefix      string
}

func NewTemporalMessageFilterProxy(workflowCtx workflow.Context, prefix string) history.MessageFilter {
	return &TemporalMessageFilterProxy{
		workflowCtx: workflowCtx,
		prefix:      prefix,
	}
}

func (t *TemporalMessageFilterProxy) Filter(ctx context.Context, msgs []messages.Message, agentID string) []messages.Message {
	var result []messages.Message
	err := workflow.ExecuteActivity(t.workflowCtx, t.prefix+"_MessageFilterActivity", msgs, agentID).Get(t.workflowCtx, &result)
	if err != nil {
		// Best-effort: keep the unfiltered bundles on activity failure.
		return msgs
	}
	return result
}
