package temporal_runtime

import (
	"context"
	"errors"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"go.temporal.io/sdk/workflow"
)

// TemporalSubAgentClient backs the agent's sub-agent listing activity, so only
// the listing enters workflow history and the registry stays on the worker.
// Calls to sub-agents need no activity of their own: call_sub_agent is one of
// the agent's tools, and runs on the worker with the real client.
type TemporalSubAgentClient struct {
	client agents.SubAgentClient
}

func NewTemporalSubAgentClient(client agents.SubAgentClient) *TemporalSubAgentClient {
	return &TemporalSubAgentClient{client: client}
}

func (c *TemporalSubAgentClient) ListSubAgents(ctx context.Context, query agents.SubAgentQuery) ([]agents.SubAgentInfo, error) {
	return c.client.ListSubAgents(ctx, query)
}

// temporalSubAgentClientProxy is the workflow-side client that schedules the
// listing activity.
type temporalSubAgentClientProxy struct {
	ctx    workflow.Context
	prefix string
}

func (s *temporalSubAgentClientProxy) ListSubAgents(_ context.Context, query agents.SubAgentQuery) ([]agents.SubAgentInfo, error) {
	var listed []agents.SubAgentInfo
	err := workflow.ExecuteActivity(s.ctx, s.prefix+"_ListSubAgents", query).Get(s.ctx, &listed)
	return listed, err
}

var errSubAgentCallInWorkflow = errors.New("sub-agents are not called from the Temporal workflow; call_sub_agent runs them on the worker")

func (s *temporalSubAgentClientProxy) RunSubAgent(context.Context, agents.SubAgentRequest) (agents.AgentTaskOutcome, error) {
	return agents.AgentTaskOutcome{}, errSubAgentCallInWorkflow
}

func (s *temporalSubAgentClientProxy) SteerSubAgent(context.Context, agents.SubAgentRequest) (bool, error) {
	return false, errSubAgentCallInWorkflow
}

var _ agents.SubAgentClient = (*temporalSubAgentClientProxy)(nil)
