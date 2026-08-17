package temporal_runtime

import (
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"go.temporal.io/sdk/workflow"
)

// TemporalDurableStep runs the agent loop's fire-and-forget lifecycle side
// effects (stream publishes) exactly once. The workflow function replays from
// the top on every workflow task, so these side effects run only at the live
// edge, gated by workflow.IsReplaying — the pattern Temporal documents for
// once-only external actions such as logging and metrics. fn must not issue
// workflow commands (it doesn't: it publishes to the broker).
type TemporalDurableStep struct {
	workflowCtx workflow.Context
}

func NewTemporalDurableStep(workflowCtx workflow.Context) *TemporalDurableStep {
	return &TemporalDurableStep{workflowCtx: workflowCtx}
}

var _ agents.DurableStep = (*TemporalDurableStep)(nil)

func (s *TemporalDurableStep) Do(fn func()) {
	if workflow.IsReplaying(s.workflowCtx) {
		return
	}
	fn()
}
