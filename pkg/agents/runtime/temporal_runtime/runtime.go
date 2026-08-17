package temporal_runtime

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"go.temporal.io/sdk/client"
)

type TemporalRuntime struct {
	client client.Client
	broker agents.StreamBroker
}

func NewTemporalRuntime(c client.Client, broker agents.StreamBroker) *TemporalRuntime {
	return &TemporalRuntime{
		client: c,
		broker: broker,
	}
}

func (r *TemporalRuntime) Run(ctx context.Context, agent *agents.Agent, in *agents.AgentInput) (*agents.AgentOutput, error) {
	if r.client == nil {
		return nil, fmt.Errorf("no temporal client available")
	}

	if in.StreamID == "" {
		in.StreamID = uuid.NewString()
	}

	run, err := r.client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		TaskQueue: "AgentWorkflowTaskQueue",
		ID:        in.StreamID,
	}, agent.Name+"_AgentWorkflow", in)
	if err != nil {
		return nil, err
	}

	var result agents.AgentOutput
	if err := run.Get(ctx, &result); err != nil {
		return nil, err
	}

	return &result, nil
}
