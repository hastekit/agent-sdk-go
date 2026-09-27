package restate_runtime

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	restate "github.com/restatedev/sdk-go"
)

// RestateSkillClient journals the agent's skill listing and reads in Restate
// run steps, so store access stays inside the step and only results are journaled.
type RestateSkillClient struct {
	restateCtx  restate.WorkflowContext
	client      agents.SkillClient
	middlewares []agents.ToolCallMiddleware
}

var _ agents.SkillClient = (*RestateSkillClient)(nil)
var _ agents.SkillReadExecutor = (*RestateSkillClient)(nil)

func NewRestateSkillClient(restateCtx restate.WorkflowContext, client agents.SkillClient, broker agents.StreamBroker, middlewares ...agents.ToolCallMiddleware) *RestateSkillClient {
	return &RestateSkillClient{
		restateCtx:  restateCtx,
		client:      client,
		middlewares: append([]agents.ToolCallMiddleware{agents.StopMiddleware{Watcher: agents.StopWatcherFrom(broker)}}, middlewares...),
	}
}

func (s *RestateSkillClient) ListSkills(_ context.Context, namespace string, rc map[string]any) ([]agents.Skill, error) {
	return restate.Run(s.restateCtx, func(ctx restate.RunContext) ([]agents.Skill, error) {
		return s.client.ListSkills(ctx, namespace, rc)
	}, restate.WithName("ListSkills"))
}

func (s *RestateSkillClient) ReadSkill(_ context.Context, namespace string, rc map[string]any, name, file string) (string, error) {
	return restate.Run(s.restateCtx, func(ctx restate.RunContext) (string, error) {
		return s.client.ReadSkill(ctx, namespace, rc, name, file)
	}, restate.WithName("ReadSkill"))
}

// ReadSkillCall runs tracing and middleware inside the step, so they fire once and never on replay.
func (s *RestateSkillClient) ReadSkillCall(_ context.Context, namespace string, rc map[string]any, name, file string, call *agents.ToolCall) (string, error) {
	return restate.Run(s.restateCtx, func(ctx restate.RunContext) (string, error) {
		content, err := agents.ReadSkillWithMiddleware(ctx, s.client, namespace, rc, name, file, call, s.middlewares)
		return content, cancellationError(err)
	}, restate.WithName("ReadSkillCall"))
}
