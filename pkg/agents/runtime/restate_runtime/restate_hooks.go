package restate_runtime

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	restate "github.com/restatedev/sdk-go"
)

// RestateToolCallHook runs a hook's methods as their own Restate steps, so
// each decision is journaled once and replayed rather than re-run — which is
// what a hook that calls out to an authorization service needs when the
// workflow is recovered.
//
// Unlike Temporal's proxy this keeps the real hook: a Restate step is a
// closure that runs in the same process, so there is nothing to register and
// nothing to send across.
type RestateHook struct {
	restateCtx  restate.WorkflowContext
	wrappedHook agents.Hook
}

var _ agents.Hook = (*RestateHook)(nil)

func NewRestateHook(restateCtx restate.WorkflowContext, wrappedHook agents.Hook) *RestateHook {
	return &RestateHook{restateCtx: restateCtx, wrappedHook: wrappedHook}
}

func (h *RestateHook) GetName() string { return h.wrappedHook.GetName() }

func (h *RestateHook) BeforeToolCall(ctx context.Context, call *agents.ToolCall) (agents.ToolCallHookResult, error) {
	res, err := restate.Run(h.restateCtx, func(restate.RunContext) (agents.ToolCallHookResult, error) {
		res, err := h.wrappedHook.BeforeToolCall(ctx, call)
		return res, abortError(err)
	}, restate.WithName(h.GetName()+"_BeforeToolCall"))
	return res, err
}

func (h *RestateHook) AfterToolCall(ctx context.Context, call *agents.ToolCall, result *agents.ToolCallResponse) (agents.ToolCallHookResult, error) {
	res, err := restate.Run(h.restateCtx, func(restate.RunContext) (agents.ToolCallHookResult, error) {
		res, err := h.wrappedHook.AfterToolCall(ctx, call, result)
		return res, abortError(err)
	}, restate.WithName(h.GetName()+"_AfterToolCall"))
	return res, err
}

func (h *RestateHook) BeforeModelCall(ctx context.Context, call *agents.ModelCall) (agents.ModelCallHookResult, error) {
	return restate.Run(h.restateCtx, func(restate.RunContext) (agents.ModelCallHookResult, error) {
		return h.wrappedHook.BeforeModelCall(ctx, call)
	}, restate.WithName(h.GetName()+"_BeforeModelCall"))
}

func (h *RestateHook) AfterModelCall(ctx context.Context, call *agents.ModelCall, result *agents.ModelCallResult) (agents.ModelCallHookResult, error) {
	return restate.Run(h.restateCtx, func(restate.RunContext) (agents.ModelCallHookResult, error) {
		return h.wrappedHook.AfterModelCall(ctx, call, result)
	}, restate.WithName(h.GetName()+"_AfterModelCall"))
}

// ToolCallAbortedErrorCode marks the run-step failure a hook reports when it
// returns an error, which ends the run. Untyped for the same reason
// ToolCancelledErrorCode is: restate's Code type is in an internal package.
//
// Not 500, tempting as it is for "the run failed": restate.ErrorCode answers 500
// for any error carrying no code of its own, so 500 here would be
// indistinguishable from every other failure to anything reading the code back.
const ToolCallAbortedErrorCode = 598

// abortError converts a hook's error into a terminal step failure. Restate
// retries the whole invocation on a non-terminal error, replaying into this step
// and asking a hook that has already said no to say it again — the run would
// hang on the refusal rather than ending on it.
//
// Only what the hook itself returned passes through here, so a failure Restate
// raises around the step keeps its own retry behaviour.
func abortError(err error) error {
	if err == nil {
		return err
	}
	return restate.TerminalError(err, ToolCallAbortedErrorCode)
}

// restateHooks wraps an agent's hooks so each of their methods runs as its own
// step.
func restateHooks(restateCtx restate.WorkflowContext, hooks []agents.Hook) []agents.Hook {
	var wrapped []agents.Hook
	for _, hook := range hooks {
		if hook == nil {
			continue
		}
		wrapped = append(wrapped, NewRestateHook(restateCtx, hook))
	}
	return wrapped
}
