package temporal_runtime

import (
	"context"
	"errors"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"go.temporal.io/sdk/workflow"
)

// TemporalSkillClient backs the agent's two skill activities. Listing and reading
// run as activities, so only their serialized results enter workflow history and
// store access or credentials stay on the worker.
type TemporalSkillClient struct {
	client      agents.SkillClient
	broker      agents.StreamBroker
	middlewares []agents.ToolCallMiddleware
}

func NewTemporalSkillClient(client agents.SkillClient, broker agents.StreamBroker, middlewares ...agents.ToolCallMiddleware) *TemporalSkillClient {
	return &TemporalSkillClient{client: client, broker: broker, middlewares: append([]agents.ToolCallMiddleware{agents.StopMiddleware{Watcher: agents.StopWatcherFrom(broker)}}, middlewares...)}
}

func (c *TemporalSkillClient) ListSkills(ctx context.Context, namespace string, rc map[string]any) ([]agents.Skill, error) {
	return c.client.ListSkills(ctx, namespace, rc)
}

// ReadSkill runs tracing and middleware inside the activity, never during replay.
func (c *TemporalSkillClient) ReadSkill(ctx context.Context, namespace string, rc map[string]any, name, file string, call *agents.ToolCall) (string, error) {
	injectProgressReporter(ctx, c.broker, call)
	content, err := agents.ReadSkillWithMiddleware(ctx, c.client, namespace, rc, name, file, call, c.middlewares)
	return content, cancellationError(err)
}

// temporalSkillClientProxy is the workflow-side client that schedules those activities.
type temporalSkillClientProxy struct {
	ctx    workflow.Context
	prefix string
}

func (s *temporalSkillClientProxy) ListSkills(_ context.Context, namespace string, rc map[string]any) ([]agents.Skill, error) {
	var skills []agents.Skill
	err := workflow.ExecuteActivity(s.ctx, s.prefix+"_ListSkills", namespace, rc).Get(s.ctx, &skills)
	return skills, err
}

// ReadSkill is not used by the workflow: read_skill carries its call through ReadSkillCall.
func (s *temporalSkillClientProxy) ReadSkill(context.Context, string, map[string]any, string, string) (string, error) {
	return "", errors.New("direct skill reads are not supported by the Temporal workflow proxy; use read_skill")
}

func (s *temporalSkillClientProxy) ReadSkillCall(_ context.Context, namespace string, rc map[string]any, name, file string, call *agents.ToolCall) (string, error) {
	var content string
	err := workflow.ExecuteActivity(s.ctx, s.prefix+"_ReadSkill", namespace, rc, name, file, call).Get(s.ctx, &content)
	return content, err
}

var _ agents.SkillClient = (*temporalSkillClientProxy)(nil)
var _ agents.SkillReadExecutor = (*temporalSkillClientProxy)(nil)
