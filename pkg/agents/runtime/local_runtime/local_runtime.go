// Package local_runtime provides a goroutine-based agent runtime that
// streams via a StreamBroker, mirroring the structure of the Temporal
// and Restate runtimes. Use it when you want broker-based streaming and
// stop-signaling for in-process agents — the no-runtime path on Agent
// runs synchronously inline and does not participate in the broker.
package local_runtime

import (
	"context"
	"errors"
	"sync"

	"github.com/google/uuid"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
)

// LocalRuntime executes the agent in a goroutine. The agent itself
// publishes chunks through the broker it was constructed with; this
// runtime only owns the goroutine boundary and StreamID generation.
type LocalRuntime struct {
	broker agents.StreamBroker

	mu      sync.RWMutex
	factory agents.AgentFactory
}

var _ agents.FactoryRuntime = (*LocalRuntime)(nil)

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

// StreamBroker returns the broker configured for this runtime.
func (r *LocalRuntime) StreamBroker() agents.StreamBroker { return r.broker }

// RegisterAgent is a no-op because local execution needs no worker registration.
func (r *LocalRuntime) RegisterAgent(options *agents.AgentOptions) error { return nil }

// RegisterAgentFactory installs the factory BuildAgent uses; builds are cached by ref.
func (r *LocalRuntime) RegisterAgentFactory(factory agents.AgentFactory) error {
	if factory == nil {
		return errors.New("agent factory is nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.factory != nil {
		return errors.New("an agent factory is already registered")
	}
	r.factory = agents.NewCachedAgentFactory(factory, 0)
	return nil
}

// BuildAgent builds ref and its handoff targets, bound to this runtime and its broker.
func (r *LocalRuntime) BuildAgent(ctx context.Context, ref agents.AgentRef) (*agents.Agent, error) {
	r.mu.RLock()
	factory := r.factory
	r.mu.RUnlock()
	if factory == nil {
		return nil, errors.New("no agent factory registered")
	}
	return agents.NewFactoryAgent(ctx, factory, ref, func(options *agents.AgentOptions) {
		options.Runtime = r
		options.StreamBroker = r.broker
	})
}
