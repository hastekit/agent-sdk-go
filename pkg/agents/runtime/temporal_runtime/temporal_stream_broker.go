package temporal_runtime

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"go.temporal.io/sdk/workflow"
)

// TemporalStreamBroker hosts the activity implementations for the
// broker calls that the agent loop makes from inside the workflow:
// IsStopped (stop signal) and DrainMessages (queued user input). Those
// hit the underlying broker (Redis, etc.) which is non-deterministic
// from a workflow's perspective, so they must run inside activities.
//
// The remaining broker methods (Publish, Subscribe, Close, Stop,
// EnqueueMessage, IsActive) are either called from outside the
// workflow or are already wrapped by other proxies (LLM publishes
// from inside an activity), so no activity wrapper is needed for
// them here.
type TemporalStreamBroker struct {
	wrappedBroker agents.StreamBroker
}

func NewTemporalStreamBroker(wrappedBroker agents.StreamBroker) *TemporalStreamBroker {
	return &TemporalStreamBroker{
		wrappedBroker: wrappedBroker,
	}
}

func (s *TemporalStreamBroker) IsStopped(ctx context.Context, channel string) (bool, error) {
	return s.wrappedBroker.IsStopped(ctx, channel)
}

func (s *TemporalStreamBroker) DrainMessages(ctx context.Context, channel string) ([]messages.Message, error) {
	return s.wrappedBroker.DrainMessages(ctx, channel)
}

// TemporalStreamBrokerProxy is the workflow-side StreamBroker. It
// routes IsStopped and DrainMessages through workflow activities so
// the loop's broker reads are durable, and delegates the rest to the
// wrapped broker (whose remaining call sites either run outside the
// workflow or are themselves inside activities).
type TemporalStreamBrokerProxy struct {
	workflowCtx   workflow.Context
	prefix        string
	wrappedBroker agents.StreamBroker
}

func NewTemporalStreamBrokerProxy(workflowCtx workflow.Context, prefix string, wrappedBroker agents.StreamBroker) agents.StreamBroker {
	return &TemporalStreamBrokerProxy{
		workflowCtx:   workflowCtx,
		prefix:        prefix,
		wrappedBroker: wrappedBroker,
	}
}

func (p *TemporalStreamBrokerProxy) Publish(ctx context.Context, channel string, chunk *responses.ResponseChunk) error {
	return p.wrappedBroker.Publish(ctx, channel, chunk)
}

func (p *TemporalStreamBrokerProxy) Subscribe(ctx context.Context, channel string) (<-chan *responses.ResponseChunk, error) {
	return p.wrappedBroker.Subscribe(ctx, channel)
}

func (p *TemporalStreamBrokerProxy) Close(ctx context.Context, channel string) error {
	return p.wrappedBroker.Close(ctx, channel)
}

func (p *TemporalStreamBrokerProxy) Stop(ctx context.Context, channel string) error {
	return p.wrappedBroker.Stop(ctx, channel)
}

func (p *TemporalStreamBrokerProxy) EnqueueMessage(ctx context.Context, channel string, msg messages.Message) error {
	return p.wrappedBroker.EnqueueMessage(ctx, channel, msg)
}

func (p *TemporalStreamBrokerProxy) IsActive(ctx context.Context, channel string) (bool, error) {
	return p.wrappedBroker.IsActive(ctx, channel)
}

func (p *TemporalStreamBrokerProxy) IsStopped(ctx context.Context, channel string) (bool, error) {
	var stopped bool
	err := workflow.ExecuteActivity(p.workflowCtx, p.prefix+"_IsStoppedActivity", channel).Get(p.workflowCtx, &stopped)
	if err != nil {
		return false, err
	}
	return stopped, nil
}

func (p *TemporalStreamBrokerProxy) DrainMessages(ctx context.Context, channel string) ([]messages.Message, error) {
	var msgs []messages.Message
	err := workflow.ExecuteActivity(p.workflowCtx, p.prefix+"_DrainMessagesActivity", channel).Get(p.workflowCtx, &msgs)
	if err != nil {
		return nil, err
	}
	return msgs, nil
}
