package agents

import (
	"context"
	"slices"

	"github.com/hastekit/hastekit-sdk-go/pkg/agents/history"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
)

// PermissionMode says how much of the tool gating applies to a run. It rides
// on AgentInput, so it is a property of the turn rather than of the agent: the
// same agent can serve a supervised turn and an unattended one.
type PermissionMode string

const (
	// PermissionModeDefault asks the user before a call whose tool was
	// declared as requiring approval, and before one whose annotations mark it
	// destructive. An empty mode means this.
	PermissionModeDefault PermissionMode = "default"

	// PermissionModeAllowAll runs every call unattended, whatever the tool
	// declared about itself. It is for turns nobody is watching — a scheduled
	// job, an eval — where a pause would simply strand the run.
	//
	// The thread's always-deny list still applies: it is the one instruction
	// that came from the user rather than from the tool.
	PermissionModeAllowAll PermissionMode = "allow_all"
)

// toolDeniedByPolicy is what a refused call reports back to the model. It says
// the decision is standing, not a transient failure, so the model stops
// retrying the same call and finds another way to finish the task.
const toolDeniedByPolicy = "Tool call denied: this tool is not permitted in this conversation. Do not try it again — complete the task another way, or tell the user it cannot be done."

// ToolPermission is the verdict for a single call.
type ToolPermission int

const (
	// ToolPermissionAllow runs the call now.
	ToolPermissionAllow ToolPermission = iota
	// ToolPermissionAsk pauses the run and surfaces an approval interrupt.
	ToolPermissionAsk
	// ToolPermissionDeny refuses the call outright and tells the model so,
	// without troubling the user.
	ToolPermissionDeny
)

// PermissionPolicy decides, for one turn, which tool calls run, which need the
// user, and which are refused. The standing lists come from the thread's meta
// and outrank Mode; Mode decides everything they don't name.
type PermissionPolicy struct {
	Mode PermissionMode

	// AlwaysAllow and AlwaysDeny are tool names the thread has a standing
	// decision about. A name in both is denied — of the two, refusing is the
	// answer that can be undone.
	AlwaysAllow []string
	AlwaysDeny  []string
}

// Decide returns the verdict for a call. tool may be nil when the model named
// something that doesn't exist: a standing decision still applies (a denied
// name stays denied), and anything else is left to the loop's own
// "tool not found" handling.
func (p PermissionPolicy) Decide(name string, tool Tool) ToolPermission {
	// The thread's standing decisions come first — they are the user's, and
	// the point of recording one is that it isn't asked again.
	if slices.Contains(p.AlwaysDeny, name) {
		return ToolPermissionDeny
	}
	if slices.Contains(p.AlwaysAllow, name) {
		return ToolPermissionAllow
	}

	if p.Mode == PermissionModeAllowAll {
		return ToolPermissionAllow
	}

	if tool == nil {
		return ToolPermissionAllow
	}

	// What the tool was configured to demand, then what it says it does. Only
	// a declared destructive hint gates a call: an absent hint means the tool
	// said nothing, and treating silence as destructive would put every
	// unannotated tool behind a prompt.
	if tool.NeedApproval() || AnnotationsOf(tool).IsDeclaredDestructive() {
		return ToolPermissionAsk
	}

	return ToolPermissionAllow
}

// Denies reports whether the thread refuses this tool outright. The tool
// execution step re-checks this: a run that paused for approval can resume a
// turn later, after the tool was denied, carrying an approval that was queued
// while it was still allowed.
func (p PermissionPolicy) Denies(name string) bool {
	return slices.Contains(p.AlwaysDeny, name)
}

// newPermissionPolicy builds the policy for a turn: the thread's standing
// decisions, read from its meta, on top of the mode the caller asked for.
func newPermissionPolicy(mode PermissionMode, permissions history.ToolPermissions) PermissionPolicy {
	if mode == "" {
		mode = PermissionModeDefault
	}
	return PermissionPolicy{
		Mode:        mode,
		AlwaysAllow: permissions.AlwaysAllow,
		AlwaysDeny:  permissions.AlwaysDeny,
	}
}

// partitionByPermission splits a round of tool calls three ways: the ones that
// run now, the ones the user has to approve first, and the ones the thread
// refuses. Denied calls are returned so the loop can still execute them — as a
// refusal the model reads, which keeps every function_call paired with an
// output.
func partitionByPermission(ctx context.Context, tools []Tool, toolCalls []responses.FunctionCallMessage, policy PermissionPolicy) (needsApproval, immediate, denied []responses.FunctionCallMessage) {
	for _, toolCall := range toolCalls {
		switch policy.Decide(toolCall.Name, findTool(ctx, tools, toolCall.Name)) {
		case ToolPermissionDeny:
			denied = append(denied, toolCall)
		case ToolPermissionAsk:
			needsApproval = append(needsApproval, toolCall)
		default:
			immediate = append(immediate, toolCall)
		}
	}
	return needsApproval, immediate, denied
}
