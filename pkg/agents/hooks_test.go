package agents_test

import (
	"context"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bothHook wraps tool calls and model calls, which one type is allowed to do.
type bothHook struct {
	name string
	log  *[]string
}

func (h *bothHook) GetName() string { return h.name }

func (h *bothHook) BeforeToolCall(ctx context.Context, call *agents.ToolCall) (agents.ToolCallHookResult, error) {
	*h.log = append(*h.log, "tool:before")
	return agents.ContinueToolCall(), nil
}

func (h *bothHook) AfterToolCall(ctx context.Context, call *agents.ToolCall, result *agents.ToolCallResponse) (agents.ToolCallHookResult, error) {
	*h.log = append(*h.log, "tool:after")
	return agents.ContinueToolCall(), nil
}

func (h *bothHook) BeforeModelCall(ctx context.Context, call *agents.ModelCall) (agents.ModelCallHookResult, error) {
	*h.log = append(*h.log, "model:before")
	return agents.ContinueModelCall(), nil
}

func (h *bothHook) AfterModelCall(ctx context.Context, call *agents.ModelCall, result *agents.ModelCallResult) (agents.ModelCallHookResult, error) {
	*h.log = append(*h.log, "model:after")
	return agents.ContinueModelCall(), nil
}

// A hook that only wraps one side embeds the no-op half — which is what makes
// "implement both" cost a line rather than four methods.
type toolOnlyHook struct {
	agents.NoopModelCallHook
	name string
	log  *[]string
}

func (h *toolOnlyHook) GetName() string { return h.name }

func (h *toolOnlyHook) BeforeToolCall(ctx context.Context, call *agents.ToolCall) (agents.ToolCallHookResult, error) {
	*h.log = append(*h.log, "tool:before")
	return agents.ContinueToolCall(), nil
}

func (h *toolOnlyHook) AfterToolCall(ctx context.Context, call *agents.ToolCall, result *agents.ToolCallResponse) (agents.ToolCallHookResult, error) {
	*h.log = append(*h.log, "tool:after")
	return agents.ContinueToolCall(), nil
}

// Both sides of a hook list are reachable, since a Hook is both.
func TestHooksOf_NarrowBothWays(t *testing.T) {
	var log []string
	hooks := []agents.Hook{&bothHook{name: "both", log: &log}, nil}

	assert.Len(t, agents.ToolCallHooksOf(hooks), 1)
	assert.Len(t, agents.ModelCallHooksOf(hooks), 1)
	assert.Nil(t, agents.ToolCallHooksOf(nil))
}

// The no-op half satisfies its side of the interface without doing anything,
// so a one-sided hook still registers.
func TestNoopHalves_SatisfyTheirSide(t *testing.T) {
	var log []string
	var _ agents.Hook = &toolOnlyHook{name: "tool-only", log: &log}

	var noop agents.NoopModelCallHook
	res, err := noop.BeforeModelCall(context.Background(), &agents.ModelCall{})
	require.NoError(t, err)
	assert.False(t, res.Handled, "a no-op half must never settle a call")

	var noopTool agents.NoopToolCallHook
	toolRes, err := noopTool.BeforeToolCall(context.Background(), &agents.ToolCall{})
	require.NoError(t, err)
	assert.False(t, toolRes.Handled)
}

// One registration, both jobs: a hook that implements both interfaces is
// consulted around the model call and around the tool call.
func TestHooks_OneHookCanWrapBoth(t *testing.T) {
	var log []string

	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "worker", "{}"),
		textResponse("done"),
	}}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Tools: []agents.Tool{newFakeTool("worker", false, "tool ran")},
		Hooks: []agents.Hook{&bothHook{name: "both", log: &log}},
	}).WithLLM(llm)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-both", Message: userMessage("go"),
	})

	requireStatus(t, out, agentstate.RunStatusCompleted)
	// First model call, then the tool it asked for, then the model call that
	// reads the result.
	assert.Equal(t, []string{
		"model:before", "model:after",
		"tool:before", "tool:after",
		"model:before", "model:after",
	}, log)
}
