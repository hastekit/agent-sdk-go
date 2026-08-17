// Package local_runtime provides a goroutine-based agent runtime that
// streams via a StreamBroker, mirroring the structure of the Temporal
// and Restate runtimes. Use it when you want broker-based streaming and
// stop-signaling for in-process agents — the no-runtime path on Agent
// runs synchronously inline and does not participate in the broker.
package local_runtime

import (
	"context"

	"github.com/google/uuid"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
)

// LocalRuntime executes the agent in a goroutine. The agent itself
// publishes chunks through the broker it was constructed with; this
// runtime only owns the goroutine boundary and StreamID generation.
type LocalRuntime struct {
	broker agents.StreamBroker
}

func NewLocalRuntime(broker agents.StreamBroker) *LocalRuntime {
	return &LocalRuntime{broker: broker}
}

// Run blocks until the agent finishes (or ctx is cancelled). The caller
// (Agent.Execute) owns the broker stream's lifecycle.
func (r *LocalRuntime) Run(ctx context.Context, agent *agents.Agent, in *agents.AgentInput) (*agents.AgentOutput, error) {
	if in.StreamID == "" {
		in.StreamID = uuid.NewString()
	}

	type result struct {
		out *agents.AgentOutput
		err error
	}
	resultCh := make(chan result, 1)

	go func() {
		out, err := agent.ExecuteLocal(ctx, in)
		resultCh <- result{out, err}
	}()

	select {
	case res := <-resultCh:
		return res.out, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
