package restate_runtime

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	restate "github.com/restatedev/sdk-go"
)

// RestateStreamBroker is the workflow-side StreamBroker proxy. IsStopped
// and DrainMessages are wrapped in restate.Run so the broker reads the
// loop makes at iteration boundaries are durable across replays. The
// remaining methods are pass-through to the wrapped broker — they are
// either called from outside the workflow (Subscribe/Stop/Enqueue/IsActive),
// or their durability is owned by the corresponding proxy (e.g. the LLM
// proxy publishes from inside its own restate.Run).
type RestateStreamBroker struct {
	restateCtx    restate.WorkflowContext
	wrappedBroker agents.StreamBroker
}

func NewRestateStreamBroker(restateCtx restate.WorkflowContext, wrappedBroker agents.StreamBroker) agents.StreamBroker {
	return &RestateStreamBroker{
		restateCtx:    restateCtx,
		wrappedBroker: wrappedBroker,
	}
}

func (b *RestateStreamBroker) Publish(ctx context.Context, channel string, chunk *responses.ResponseChunk) error {
	return b.wrappedBroker.Publish(ctx, channel, chunk)
}

func (b *RestateStreamBroker) Subscribe(ctx context.Context, channel string) (<-chan *responses.ResponseChunk, error) {
	return b.wrappedBroker.Subscribe(ctx, channel)
}

func (b *RestateStreamBroker) Close(ctx context.Context, channel string) error {
	return b.wrappedBroker.Close(ctx, channel)
}

func (b *RestateStreamBroker) Stop(ctx context.Context, channel string) error {
	return b.wrappedBroker.Stop(ctx, channel)
}

func (b *RestateStreamBroker) EnqueueMessage(ctx context.Context, channel string, msg messages.Message) error {
	return b.wrappedBroker.EnqueueMessage(ctx, channel, msg)
}

func (b *RestateStreamBroker) IsActive(ctx context.Context, channel string) (bool, error) {
	return b.wrappedBroker.IsActive(ctx, channel)
}

func (b *RestateStreamBroker) IsStopped(ctx context.Context, channel string) (bool, error) {
	return restate.Run(b.restateCtx, func(ctx restate.RunContext) (bool, error) {
		return b.wrappedBroker.IsStopped(ctx, channel)
	}, restate.WithName("IsStopped"))
}

func (b *RestateStreamBroker) DrainMessages(ctx context.Context, channel string) ([]messages.Message, error) {
	return restate.Run(b.restateCtx, func(ctx restate.RunContext) ([]messages.Message, error) {
		return b.wrappedBroker.DrainMessages(ctx, channel)
	}, restate.WithName("DrainMessages"))
}
