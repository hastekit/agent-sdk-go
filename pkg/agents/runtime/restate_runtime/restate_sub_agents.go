package restate_runtime

import (
	"context"
	"errors"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	restate "github.com/restatedev/sdk-go"
)

// RestateSubAgentClient journals the agent's sub-agent listing in a Restate run
// step, so the registry is read inside the step and only the listing is
// journaled. Calls to sub-agents go through no step of their own:
// call_sub_agent is one of the agent's tools, and runs with the real client.
type RestateSubAgentClient struct {
	restateCtx restate.WorkflowContext
	client     agents.SubAgentClient
}

var _ agents.SubAgentClient = (*RestateSubAgentClient)(nil)

func NewRestateSubAgentClient(restateCtx restate.WorkflowContext, client agents.SubAgentClient) *RestateSubAgentClient {
	return &RestateSubAgentClient{restateCtx: restateCtx, client: client}
}

func (s *RestateSubAgentClient) ListSubAgents(_ context.Context, query agents.SubAgentQuery) ([]agents.SubAgentInfo, error) {
	return restate.Run(s.restateCtx, func(ctx restate.RunContext) ([]agents.SubAgentInfo, error) {
		return s.client.ListSubAgents(ctx, query)
	}, restate.WithName("ListSubAgents"))
}

var errSubAgentCallInWorkflow = errors.New("sub-agents are not called from the Restate workflow; call_sub_agent runs them with the real client")

func (s *RestateSubAgentClient) RunSubAgent(context.Context, agents.SubAgentRequest) (agents.AgentTaskOutcome, error) {
	return agents.AgentTaskOutcome{}, errSubAgentCallInWorkflow
}

func (s *RestateSubAgentClient) SteerSubAgent(context.Context, agents.SubAgentRequest) (bool, error) {
	return false, errSubAgentCallInWorkflow
}
