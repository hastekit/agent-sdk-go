package temporal_agent_builder

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentconfig"
	"go.temporal.io/sdk/client"
)

// Runtime is the caller side: an agentconfig.Runtime that starts the
// AgentBuilder workflow. agentconfig.Builder binds each agent it builds to the
// run for that agent's snapshot, through the plain AgentOptions.Runtime field.
type Runtime struct {
	client client.Client
	broker agents.StreamBroker
}

var _ agentconfig.Runtime = (*Runtime)(nil)

// NewRuntime starts workflows with c. broker must be the one the workers
// publish on.
func NewRuntime(c client.Client, broker agents.StreamBroker) *Runtime {
	return &Runtime{client: c, broker: broker}
}

// StreamBroker is the broker the workflows publish on.
func (r *Runtime) StreamBroker() agents.StreamBroker { return r.broker }

// Bind returns the runtime that runs snapshot.
func (r *Runtime) Bind(snapshot *agentconfig.Snapshot) agents.Runtime {
	return &snapshotRun{runtime: r, snapshot: snapshot}
}

// snapshotRun is one config-built agent's runtime.
type snapshotRun struct {
	runtime  *Runtime
	snapshot *agentconfig.Snapshot
}

func (s *snapshotRun) StreamBroker() agents.StreamBroker        { return s.runtime.broker }
func (s *snapshotRun) RegisterAgent(*agents.AgentOptions) error { return nil }

// Run starts the workflow keyed by the run's stream, as static agents' runs
// are, and waits for its output.
func (s *snapshotRun) Run(ctx context.Context, _ *agents.Agent, in *agents.AgentInput) (*agents.AgentOutput, error) {
	if s.runtime.client == nil {
		return nil, errors.New("no temporal client available")
	}
	if in.StreamID == "" {
		in.StreamID = uuid.NewString()
	}
	run, err := s.runtime.client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		TaskQueue: TaskQueue,
		ID:        in.StreamID,
	}, WorkflowName, s.snapshot, in)
	if err != nil {
		return nil, err
	}
	var result agents.AgentOutput
	if err := run.Get(ctx, &result); err != nil {
		return nil, err
	}
	return &result, nil
}
