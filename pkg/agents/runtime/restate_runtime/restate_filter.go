package restate_runtime

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	restate "github.com/restatedev/sdk-go"
)

// RestateMessageFilter wraps a history.MessageFilter so its transform runs
// inside restate.Run
type RestateMessageFilter struct {
	restateCtx    restate.WorkflowContext
	wrappedFilter history.MessageFilter
}

func NewRestateMessageFilter(restateCtx restate.WorkflowContext, wrappedFilter history.MessageFilter) *RestateMessageFilter {
	return &RestateMessageFilter{
		restateCtx:    restateCtx,
		wrappedFilter: wrappedFilter,
	}
}

func (t *RestateMessageFilter) Filter(ctx context.Context, msgs []messages.Message, agentID string) []messages.Message {
	out, err := restate.Run(t.restateCtx, func(ctx restate.RunContext) ([]messages.Message, error) {
		return t.wrappedFilter.Filter(ctx, msgs, agentID), nil
	}, restate.WithName("FilterMessages"))
	if err != nil {
		// Best-effort: on a durable-step failure, fall back to the
		// unfiltered bundles rather than failing the run.
		return msgs
	}
	return out
}
