package temporal_runtime

import (
	"context"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"go.temporal.io/sdk/workflow"
)

// Skill listing and resolution are activities, so only their serialized results
// enter workflow history. Adding skills does not require registering activities.
type temporalSkillSet struct {
	ctx    workflow.Context
	name   string
	prefix string
}

func (s *temporalSkillSet) GetName() string { return s.name }
func (s *temporalSkillSet) ListSkills(_ context.Context, namespace string, rc map[string]any) ([]agents.Skill, error) {
	var skills []agents.Skill
	err := workflow.ExecuteActivity(s.ctx, s.prefix+"_ListSkills", namespace, rc).Get(s.ctx, &skills)
	return skills, err
}
func (s *temporalSkillSet) ResolveSkill(_ context.Context, namespace string, rc map[string]any, name, file string) (string, error) {
	var content string
	err := workflow.ExecuteActivity(s.ctx, s.prefix+"_ResolveSkill", namespace, rc, name, file).Get(s.ctx, &content)
	return content, err
}

func (s *temporalSkillSet) ResolveSkillCall(_ context.Context, namespace string, rc map[string]any, name, file string, call *agents.ToolCall) (string, error) {
	var content string
	err := workflow.ExecuteActivity(s.ctx, s.prefix+"_ReadSkill", namespace, rc, name, file, call).Get(s.ctx, &content)
	return content, err
}

type temporalSkillReader struct {
	set         agents.SkillSet
	broker      agents.StreamBroker
	middlewares []agents.ToolCallMiddleware
}

func (s *temporalSkillReader) Read(ctx context.Context, namespace string, rc map[string]any, name, file string, call *agents.ToolCall) (string, error) {
	injectProgressReporter(ctx, s.broker, call)
	content, err := agents.ResolveSkillWithMiddleware(ctx, s.set, namespace, rc, name, file, call, s.middlewares)
	return content, cancellationError(err)
}
