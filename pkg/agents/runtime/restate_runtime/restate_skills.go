package restate_runtime

import (
	"context"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	restate "github.com/restatedev/sdk-go"
)

func restateSkillSet(ctx restate.WorkflowContext, set agents.SkillSet, broker agents.StreamBroker, middlewares []agents.ToolCallMiddleware) agents.SkillSet {
	proxy := agents.SkillSetFuncs{
		Name: set.GetName(),
		List: func(_ context.Context, namespace string, rc map[string]any) ([]agents.Skill, error) {
			return restate.Run(ctx, func(runCtx restate.RunContext) ([]agents.Skill, error) { return set.ListSkills(runCtx, namespace, rc) }, restate.WithName("ListSkills:"+set.GetName()))
		},
		Resolve: func(_ context.Context, namespace string, rc map[string]any, name, file string) (string, error) {
			return restate.Run(ctx, func(runCtx restate.RunContext) (string, error) {
				return set.ResolveSkill(runCtx, namespace, rc, name, file)
			}, restate.WithName("ResolveSkill:"+set.GetName()))
		},
	}
	return &restateSkillReader{SkillSetFuncs: proxy, ctx: ctx, set: set, middlewares: append([]agents.ToolCallMiddleware{agents.StopMiddleware{Watcher: agents.StopWatcherFrom(broker)}}, middlewares...)}
}

type restateSkillReader struct {
	agents.SkillSetFuncs
	ctx         restate.WorkflowContext
	set         agents.SkillSet
	middlewares []agents.ToolCallMiddleware
}

func (s *restateSkillReader) ResolveSkillCall(_ context.Context, namespace string, rc map[string]any, name, file string, call *agents.ToolCall) (string, error) {
	return restate.Run(s.ctx, func(ctx restate.RunContext) (string, error) {
		content, err := agents.ResolveSkillWithMiddleware(ctx, s.set, namespace, rc, name, file, call, s.middlewares)
		return content, cancellationError(err)
	}, restate.WithName("ReadSkill:"+s.GetName()))
}
