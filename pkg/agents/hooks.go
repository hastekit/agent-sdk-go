package agents

import "context"

// Hook is anything an agent can be given to observe or intercept what it does.
// It is the union of the specific hook interfaces, composed by embedding:
//
//   - ToolCallHook wraps every tool call the agent makes
//   - ModelCallHook wraps every call it makes to the model
//
// A Hook therefore implements both, and the compiler says so at the point of
// registration. That is the reason for composing rather than accepting a
// marker interface and sorting by type assertion: a hook whose method drifted
// out of shape would still satisfy a marker, register cleanly, and then never
// run — a failure that shows up as nothing happening.
//
// Implementing a side you don't care about costs one embedded field, not four
// methods. A budget check that has no interest in tools reads:
//
//	type credits struct{ agents.NoopToolCallHook }
//
//	func (c *credits) GetName() string { return "credits" }
//	func (c *credits) BeforeModelCall(...) { ... }
//	func (c *credits) AfterModelCall(...)  { ... }
type Hook interface {
	ToolCallHook
	ModelCallHook
}

// NoopToolCallHook is the tool-call half of a Hook, doing nothing. Embed it in
// a hook that only wraps model calls.
type NoopToolCallHook struct{}

func (NoopToolCallHook) BeforeToolCall(context.Context, *ToolCall) (ToolCallHookResult, error) {
	return ContinueToolCall(), nil
}

func (NoopToolCallHook) AfterToolCall(context.Context, *ToolCall, *ToolCallResponse) (ToolCallHookResult, error) {
	return ContinueToolCall(), nil
}

// NoopModelCallHook is the model-call half of a Hook, doing nothing. Embed it
// in a hook that only wraps tool calls.
type NoopModelCallHook struct{}

func (NoopModelCallHook) BeforeModelCall(context.Context, *ModelCall) (ModelCallHookResult, error) {
	return ContinueModelCall(), nil
}

func (NoopModelCallHook) AfterModelCall(context.Context, *ModelCall, *ModelCallResult) (ModelCallHookResult, error) {
	return ContinueModelCall(), nil
}

// ToolCallHooksOf narrows a hook list to the tool-call side, which is what the
// executor runs. Go has no covariance for slices, so the conversion is a loop.
func ToolCallHooksOf(hooks []Hook) []ToolCallHook {
	if len(hooks) == 0 {
		return nil
	}
	out := make([]ToolCallHook, 0, len(hooks))
	for _, hook := range hooks {
		if hook != nil {
			out = append(out, hook)
		}
	}
	return out
}

// ModelCallHooksOf narrows a hook list to the model-call side, which is what
// the agent loop runs.
func ModelCallHooksOf(hooks []Hook) []ModelCallHook {
	if len(hooks) == 0 {
		return nil
	}
	out := make([]ModelCallHook, 0, len(hooks))
	for _, hook := range hooks {
		if hook != nil {
			out = append(out, hook)
		}
	}
	return out
}
