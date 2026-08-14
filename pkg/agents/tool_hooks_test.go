package agents_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hastekit/hastekit-sdk-go/pkg/agents"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/hastekit-sdk-go/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tracingHook records the phases it ran in, so a test can assert on nesting
// and order.
type tracingHook struct {
	agents.NoopModelCallHook

	name   string
	log    *[]string
	before func(call *agents.ToolCall) (agents.ToolCallHookResult, error)
	after  func(call *agents.ToolCall, result *agents.ToolCallResponse) (agents.ToolCallHookResult, error)
}

func (h *tracingHook) GetName() string { return h.name }

func (h *tracingHook) BeforeToolCall(ctx context.Context, call *agents.ToolCall) (agents.ToolCallHookResult, error) {
	*h.log = append(*h.log, "before:"+h.name)
	if h.before == nil {
		return agents.ContinueToolCall(), nil
	}
	return h.before(call)
}

func (h *tracingHook) AfterToolCall(ctx context.Context, call *agents.ToolCall, result *agents.ToolCallResponse) (agents.ToolCallHookResult, error) {
	*h.log = append(*h.log, "after:"+h.name)
	if h.after == nil {
		return agents.ContinueToolCall(), nil
	}
	return h.after(call, result)
}

// An agent's hooks wrap every tool it calls, not just an MCP server's.
func TestAgentToolCallHooks_WrapAPlainTool(t *testing.T) {
	var log []string

	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "worker", "{}"),
		textResponse("done"),
	}}
	tool := newFakeTool("worker", false, "tool ran")

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Tools: []agents.Tool{tool},
		Hooks: []agents.Hook{&tracingHook{name: "audit", log: &log}},
	}).WithLLM(llm)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-agent-hooks", Message: userMessage("go"),
	})

	requireStatus(t, out, agentstate.RunStatusCompleted)
	assert.Equal(t, []string{"before:audit", "after:audit"}, log)
	assert.Equal(t, 1, tool.callCount())
}

// A hook can refuse a call to any tool, not only an MCP one.
func TestAgentToolCallHooks_CanRefuseAnyTool(t *testing.T) {
	var log []string

	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "worker", "{}"),
		textResponse("understood"),
	}}
	tool := newFakeTool("worker", false, "tool ran")

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Tools: []agents.Tool{tool},
		Hooks: []agents.Hook{&tracingHook{
			name: "authz",
			log:  &log,
			before: func(call *agents.ToolCall) (agents.ToolCallHookResult, error) {
				return agents.ContinueToolCall(), errors.New("not allowed for this user")
			},
		}},
	}).WithLLM(llm)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-agent-refuse", Message: userMessage("go"),
	})

	requireStatus(t, out, agentstate.RunStatusCompleted)
	assert.Equal(t, 0, tool.callCount(), "a refused tool must not run")
	assert.Contains(t, messagesText(out.Output), "not allowed for this user")
}

// The agent hands its hooks to the executor, which is what runs them. Nothing
// fails loudly if that injection is missed — the hooks just never run — so
// assert the executor actually received them.
func TestAgentToolCallHooks_InjectedIntoTheExecutor(t *testing.T) {
	hook := &tracingHook{name: "audit", log: new([]string)}
	executor := &agents.DefaultToolExecutor{}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:         "main",
		ToolExecutor: executor,
		Hooks:        []agents.Hook{hook},
	})

	bound, ok := agent.ToolExecutor().(*agents.DefaultToolExecutor)
	require.True(t, ok)
	require.Len(t, bound.Hooks, 1)
	assert.Equal(t, "audit", bound.Hooks[0].GetName())

	// Bound to a copy, so an executor shared between agents never runs another
	// agent's hooks.
	assert.Empty(t, executor.Hooks)
}

// An executor built with hooks of its own keeps them when the agent adds none.
func TestAgentToolCallHooks_DoNotClearTheExecutorsOwn(t *testing.T) {
	hook := &tracingHook{name: "executor-own", log: new([]string)}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:         "main",
		ToolExecutor: &agents.DefaultToolExecutor{Hooks: []agents.ToolCallHook{hook}},
	})

	bound, ok := agent.ToolExecutor().(*agents.DefaultToolExecutor)
	require.True(t, ok)
	require.Len(t, bound.Hooks, 1)
	assert.Equal(t, "executor-own", bound.Hooks[0].GetName())
}

// The zero value passes the call forward, so a hook that has nothing to say
// needs no ceremony.
func TestToolCallHookResult_ZeroValueContinues(t *testing.T) {
	assert.Equal(t, agents.ToolCallHookResult{}, agents.ContinueToolCall())
	assert.False(t, agents.ContinueToolCall().Handled)

	handled := agents.HandleToolCall(nil)
	assert.True(t, handled.Handled, "handled with no response is still handled")
	assert.Nil(t, handled.Response)
}

// runHooks drives a set of hooks around a tool that records whether it ran —
// the same thing an executor does, without one in the way.
func runHooks(t *testing.T, hooks []agents.ToolCallHook, output string) (*agents.ToolCallResponse, bool) {
	t.Helper()

	toolRan := false
	call := &agents.ToolCall{
		FunctionCallMessage: &responses.FunctionCallMessage{ID: "fc_1", CallID: "call_1", Name: "worker"},
	}

	resp, err := agents.RunWithToolCallHooks(t.Context(), hooks, call,
		func(context.Context, *agents.ToolCall) (*agents.ToolCallResponse, error) {
			toolRan = true
			return agents.ToolCallResult(call, output), nil
		})
	require.NoError(t, err)
	return resp, toolRan
}

// The flag is the answer, not the response. "I handled this, and the answer is
// nothing to say" is a different thing from "carry on without me", and a
// caller reading only the response cannot tell them apart.
func TestToolCallHookResult_HandledWithNothingStillAnswersTheCall(t *testing.T) {
	var log []string

	resp, toolRan := runHooks(t, []agents.ToolCallHook{&tracingHook{
		name: "silent",
		log:  &log,
		before: func(call *agents.ToolCall) (agents.ToolCallHookResult, error) {
			return agents.HandleToolCall(nil), nil
		},
	}}, "tool ran")

	assert.False(t, toolRan, "handled means the tool does not run")
	// The call still has to be answered, or the next request to the provider
	// carries a function_call with no output.
	require.NotNil(t, resp.FunctionCallOutputMessage)
	assert.Equal(t, "call_1", resp.CallID)
	assert.Equal(t, "", *resp.Output.OfString)
}

// A hook that handles the call settles it: the tool is skipped and no later
// before-hook is asked.
func TestToolCallHooks_HandledSkipsTheToolAndLaterHooks(t *testing.T) {
	var log []string

	resp, toolRan := runHooks(t, []agents.ToolCallHook{
		&tracingHook{
			name: "cache",
			log:  &log,
			before: func(call *agents.ToolCall) (agents.ToolCallHookResult, error) {
				return agents.HandleToolCall(agents.ToolCallResult(call, "answered from cache")), nil
			},
		},
		&tracingHook{name: "later", log: &log},
	}, "tool ran")

	assert.False(t, toolRan)
	assert.Equal(t, "answered from cache", *resp.Output.OfString)
	assert.NotContains(t, log, "before:later", "a settled call must not reach a later BeforeToolCall")
	assert.Contains(t, log, "after:later", "the after phase still runs for every hook")
}

// After-hooks see the result even when a before-hook produced it, so an audit
// hook records refused calls too.
func TestToolCallHooks_AfterSeesARefusedCall(t *testing.T) {
	var log []string
	var seen string

	runHooks(t, []agents.ToolCallHook{
		&tracingHook{
			name: "authz",
			log:  &log,
			before: func(call *agents.ToolCall) (agents.ToolCallHookResult, error) {
				return agents.ContinueToolCall(), errors.New("denied")
			},
		},
		&tracingHook{
			name: "audit",
			log:  &log,
			after: func(call *agents.ToolCall, result *agents.ToolCallResponse) (agents.ToolCallHookResult, error) {
				seen = *result.Output.OfString
				return agents.ContinueToolCall(), nil
			},
		},
	}, "tool ran")

	assert.Equal(t, "denied", seen)
}

// An after-hook can replace the result — redaction, or a post-filter that must
// not let something through. An error from one replaces it too, failing closed.
func TestToolCallHooks_AfterCanReplaceTheResult(t *testing.T) {
	var log []string

	resp, _ := runHooks(t, []agents.ToolCallHook{&tracingHook{
		name: "redact",
		log:  &log,
		after: func(call *agents.ToolCall, result *agents.ToolCallResponse) (agents.ToolCallHookResult, error) {
			return agents.HandleToolCall(agents.ToolCallResult(call, "[redacted]")), nil
		},
	}}, "secret")
	assert.Equal(t, "[redacted]", *resp.Output.OfString)

	resp, _ = runHooks(t, []agents.ToolCallHook{&tracingHook{
		name: "filter",
		log:  &log,
		after: func(call *agents.ToolCall, result *agents.ToolCallResponse) (agents.ToolCallHookResult, error) {
			return agents.ContinueToolCall(), errors.New("result withheld")
		},
	}}, "secret")
	assert.Equal(t, "result withheld", *resp.Output.OfString)
}

// A paused call has no result yet, so the after-hooks wait for the resume
// rather than running twice for one call.
func TestToolCallHooks_AfterSkippedOnAPause(t *testing.T) {
	var log []string

	resp, toolRan := runHooks(t, []agents.ToolCallHook{&tracingHook{
		name: "pause",
		log:  &log,
		before: func(call *agents.ToolCall) (agents.ToolCallHookResult, error) {
			return agents.HandleToolCall(&agents.ToolCallResponse{Interrupts: []responses.Interrupt{{
				FunctionCallMessage: *call.FunctionCallMessage,
				Mode:                responses.InterruptModeURL,
			}}}), nil
		},
	}}, "tool ran")

	assert.False(t, toolRan)
	require.Len(t, resp.Interrupts, 1)
	assert.Nil(t, resp.FunctionCallOutputMessage, "a pause has no result yet")
	assert.NotContains(t, log, "after:pause", "the after-hook belongs to the resumed call")
}

// A hand-built response can arrive without the ids that pair it with the
// function_call in history. Leaving it unpaired would break the next request to
// the provider rather than the hook that caused it.
func TestToolCallHooks_StampIdsOnAHandBuiltResponse(t *testing.T) {
	var log []string

	resp, _ := runHooks(t, []agents.ToolCallHook{&tracingHook{
		name: "settle",
		log:  &log,
		before: func(call *agents.ToolCall) (agents.ToolCallHookResult, error) {
			return agents.HandleToolCall(&agents.ToolCallResponse{
				FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
					Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr("hand built")},
				},
			}), nil
		},
	}}, "tool ran")

	assert.Equal(t, "hand built", *resp.Output.OfString)
	assert.Equal(t, "call_1", resp.CallID)
	assert.Equal(t, "fc_1", resp.ID)
}
