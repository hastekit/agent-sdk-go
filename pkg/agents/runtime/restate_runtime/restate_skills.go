package restate_runtime

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	restate "github.com/restatedev/sdk-go"
)

// RestateSkillSet journals skill listing and resolution in Restate workflow steps.
type RestateSkillSet struct {
	restateCtx      restate.WorkflowContext
	wrappedSkillSet agents.SkillSet
	middlewares     []agents.ToolCallMiddleware
}

var _ agents.SkillSet = (*RestateSkillSet)(nil)
var _ agents.SkillReadExecutor = (*RestateSkillSet)(nil)

func NewRestateSkillSet(restateCtx restate.WorkflowContext, set agents.SkillSet, broker agents.StreamBroker, middlewares ...agents.ToolCallMiddleware) *RestateSkillSet {
	return &RestateSkillSet{
		restateCtx:      restateCtx,
		wrappedSkillSet: set,
		middlewares:     append([]agents.ToolCallMiddleware{agents.StopMiddleware{Watcher: agents.StopWatcherFrom(broker)}}, middlewares...),
	}
}

func (s *RestateSkillSet) GetName() string { return s.wrappedSkillSet.GetName() }

func (s *RestateSkillSet) ListSkills(_ context.Context, namespace string, rc map[string]any) ([]agents.Skill, error) {
	return restate.Run(s.restateCtx, func(ctx restate.RunContext) ([]agents.Skill, error) {
		return s.wrappedSkillSet.ListSkills(ctx, namespace, rc)
	}, restate.WithName("ListSkills:"+s.GetName()))
}

func (s *RestateSkillSet) ResolveSkill(_ context.Context, namespace string, rc map[string]any, name, file string) (string, error) {
	return restate.Run(s.restateCtx, func(ctx restate.RunContext) (string, error) {
		return s.wrappedSkillSet.ResolveSkill(ctx, namespace, rc, name, file)
	}, restate.WithName("ResolveSkill:"+s.GetName()))
}

func (s *RestateSkillSet) ResolveSkillCall(_ context.Context, namespace string, rc map[string]any, name, file string, call *agents.ToolCall) (string, error) {
	return restate.Run(s.restateCtx, func(ctx restate.RunContext) (string, error) {
		content, err := agents.ResolveSkillWithMiddleware(ctx, s.wrappedSkillSet, namespace, rc, name, file, call, s.middlewares)
		return content, cancellationError(err)
	}, restate.WithName("ReadSkill:"+s.GetName()))
}
