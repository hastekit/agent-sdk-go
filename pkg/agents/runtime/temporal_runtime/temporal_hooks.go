package temporal_runtime

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"go.temporal.io/sdk/workflow"
)

// Activity name suffixes for a hook's two methods. Each is registered and
// executed separately so a hook that calls out to another service is journaled
// on the way out and replayed on the way back, rather than re-run.
const (
	beforeToolCallActivitySuffix = "_BeforeToolCallActivity"
	afterToolCallActivitySuffix  = "_AfterToolCallActivity"

	beforeModelCallActivitySuffix = "_BeforeModelCallActivity"
	afterModelCallActivitySuffix  = "_AfterModelCallActivity"
)

// hookActivityName is the activity a hook's methods are registered under. It
// is scoped to the agent, like every other activity here, because a worker
// resolves activity names globally: two agents that both name a hook "authz"
// would otherwise register the same name twice, and the second registration
// silently replaces the first — leaving one agent's calls checked by the
// other's hook, with the other's configuration.
func hookActivityName(agentName, hookName string) string {
	return agentName + "_" + hookName
}

// hookActivities returns the activities to register for a set of hooks — four
// per hook, scoped to the agent.
//
// Hook names must be unique within an agent and stable across deploys:
// renaming one orphans in-flight runs the same way renaming any activity does.
func hookActivities(agentName string, hooks []agents.Hook) map[string]any {
	activities := map[string]any{}
	for _, hook := range hooks {
		if hook == nil {
			continue
		}
		name := hookActivityName(agentName, hook.GetName())
		activities[name+beforeToolCallActivitySuffix] = hook.BeforeToolCall
		activities[name+afterToolCallActivitySuffix] = hook.AfterToolCall
		activities[name+beforeModelCallActivitySuffix] = hook.BeforeModelCall
		activities[name+afterModelCallActivitySuffix] = hook.AfterModelCall
	}
	return activities
}

// TemporalHookProxy is a hook as seen from inside a workflow: each of its four
// methods runs as its own activity, so the decision it returns is journaled
// once and replayed thereafter. The real hook — which may hold a database
// handle or an HTTP client, and cannot cross the workflow boundary — runs on
// the activity side.
type TemporalHookProxy struct {
	workflowCtx workflow.Context
	name        string
}

var _ agents.Hook = (*TemporalHookProxy)(nil)

func NewTemporalHookProxy(workflowCtx workflow.Context, name string) *TemporalHookProxy {
	return &TemporalHookProxy{workflowCtx: workflowCtx, name: name}
}

func (h *TemporalHookProxy) GetName() string { return h.name }

func (h *TemporalHookProxy) BeforeToolCall(ctx context.Context, call *agents.ToolCall) (agents.ToolCallHookResult, error) {
	var out agents.ToolCallHookResult
	err := workflow.ExecuteActivity(h.workflowCtx, h.name+beforeToolCallActivitySuffix, call).Get(h.workflowCtx, &out)
	if err != nil {
		return agents.ContinueToolCall(), err
	}
	return out, nil
}

func (h *TemporalHookProxy) AfterToolCall(ctx context.Context, call *agents.ToolCall, result *agents.ToolCallResponse) (agents.ToolCallHookResult, error) {
	var out agents.ToolCallHookResult
	err := workflow.ExecuteActivity(h.workflowCtx, h.name+afterToolCallActivitySuffix, call, result).Get(h.workflowCtx, &out)
	if err != nil {
		return agents.ContinueToolCall(), err
	}
	return out, nil
}

func (h *TemporalHookProxy) BeforeModelCall(ctx context.Context, call *agents.ModelCall) (agents.ModelCallHookResult, error) {
	var out agents.ModelCallHookResult
	err := workflow.ExecuteActivity(h.workflowCtx, h.name+beforeModelCallActivitySuffix, call).Get(h.workflowCtx, &out)
	if err != nil {
		return agents.ContinueModelCall(), err
	}
	return out, nil
}

func (h *TemporalHookProxy) AfterModelCall(ctx context.Context, call *agents.ModelCall, result *agents.ModelCallResult) (agents.ModelCallHookResult, error) {
	var out agents.ModelCallHookResult
	err := workflow.ExecuteActivity(h.workflowCtx, h.name+afterModelCallActivitySuffix, call, result).Get(h.workflowCtx, &out)
	if err != nil {
		return agents.ContinueModelCall(), err
	}
	return out, nil
}

// hookProxies builds the workflow-side stand-ins for an agent's hooks, in the
// order they were configured.
func hookProxies(workflowCtx workflow.Context, agentName string, hooks []agents.Hook) []agents.Hook {
	if len(hooks) == 0 {
		return nil
	}
	proxies := make([]agents.Hook, 0, len(hooks))
	for _, hook := range hooks {
		if hook == nil {
			continue
		}
		proxies = append(proxies, NewTemporalHookProxy(workflowCtx, hookActivityName(agentName, hook.GetName())))
	}
	return proxies
}
