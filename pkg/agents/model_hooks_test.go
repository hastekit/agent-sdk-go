package agents_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// budgetHook stands in for a check against a balance somebody keeps elsewhere.
type budgetHook struct {
	agents.NoopToolCallHook

	name    string
	log     *[]string
	before  func(call *agents.ModelCall) (agents.ModelCallHookResult, error)
	after   func(call *agents.ModelCall, result *agents.ModelCallResult) (agents.ModelCallHookResult, error)
	seen    []*agents.ModelCall
	charged []responses.Usage
}

func (h *budgetHook) GetName() string { return h.name }

func (h *budgetHook) BeforeModelCall(ctx context.Context, call *agents.ModelCall) (agents.ModelCallHookResult, error) {
	*h.log = append(*h.log, "before:"+h.name)
	h.seen = append(h.seen, call)
	if h.before == nil {
		return agents.ContinueModelCall(), nil
	}
	return h.before(call)
}

func (h *budgetHook) AfterModelCall(ctx context.Context, call *agents.ModelCall, result *agents.ModelCallResult) (agents.ModelCallHookResult, error) {
	*h.log = append(*h.log, "after:"+h.name)
	h.charged = append(h.charged, result.Usage)
	if h.after == nil {
		return agents.ContinueModelCall(), nil
	}
	return h.after(call, result)
}

// The hooks run around every call to the model, once per loop iteration.
func TestModelCallHooks_RunAroundEveryCall(t *testing.T) {
	var log []string

	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "worker", "{}"),
		textResponse("done"),
	}}
	hook := &budgetHook{name: "budget", log: &log}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Tools: []agents.Tool{newFakeTool("worker", false, "tool ran")},
		Hooks: []agents.Hook{hook},
	}).WithLLM(llm)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-model-hooks", Message: userMessage("go"),
	})

	requireStatus(t, out, agentstate.RunStatusCompleted)
	// Two model calls — the one that asked for the tool, and the one that read
	// its result — so two rounds of hooks.
	assert.Equal(t, []string{
		"before:budget", "after:budget", "before:budget", "after:budget",
	}, log)
	assert.Equal(t, 2, llm.callCount())
}

// The before hook is told what the call is and who it belongs to, which is
// what a balance is looked up by.
func TestModelCallHooks_SeeTheCallAndItsRun(t *testing.T) {
	var log []string

	llm := &scriptedLLM{script: []*responses.Response{textResponse("done")}}
	hook := &budgetHook{name: "budget", log: &log}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Hooks: []agents.Hook{hook},
	}).WithLLM(llm)

	runAgent(t, agent, &agents.AgentInput{
		Namespace:  "test",
		ThreadID:   "thread-model-identity",
		Message:    userMessage("go"),
		RunContext: map[string]any{"tenant": "acme"},
	})

	require.Len(t, hook.seen, 1)
	call := hook.seen[0]
	assert.Equal(t, "main", call.AgentName)
	assert.Equal(t, "test", call.Namespace)
	assert.Equal(t, "thread-model-identity", call.ThreadID)
	assert.Equal(t, "acme", call.RunContext["tenant"])
	assert.Equal(t, 0, call.LoopIteration, "the run's own counter, which starts at zero")
}

// An exhausted budget answers for the model: the provider is never called, the
// user gets a message they can read, and the run completes rather than fails.
func TestModelCallHooks_HandledAnswersForTheModel(t *testing.T) {
	var log []string

	llm := &scriptedLLM{script: []*responses.Response{}}
	hook := &budgetHook{
		name: "budget",
		log:  &log,
		before: func(call *agents.ModelCall) (agents.ModelCallHookResult, error) {
			return agents.HandleModelCall(agents.ModelCallText("You are out of credits.")), nil
		},
	}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Hooks: []agents.Hook{hook},
	}).WithLLM(llm)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-out-of-credit", Message: userMessage("go"),
	})

	requireStatus(t, out, agentstate.RunStatusCompleted)
	assert.Equal(t, 0, llm.callCount(), "the provider must not be called")
	assert.Contains(t, messagesText(out.Output), "You are out of credits.")
}

// A refusal that has nothing to say fails the run instead.
func TestModelCallHooks_ErrorFailsTheRun(t *testing.T) {
	var log []string

	llm := &scriptedLLM{script: []*responses.Response{}}
	hook := &budgetHook{
		name: "budget",
		log:  &log,
		before: func(call *agents.ModelCall) (agents.ModelCallHookResult, error) {
			return agents.ContinueModelCall(), errors.New("billing unavailable")
		},
	}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Hooks: []agents.Hook{hook},
	}).WithLLM(llm)

	handle, err := agent.Execute(context.Background(), &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-billing-down", Message: userMessage("go"),
	})
	require.NoError(t, err)

	_, runErr := handle.Result()
	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "billing unavailable")
	assert.Equal(t, 0, llm.callCount())
}

// The after hook is told what the call reported using — the number a balance
// is drawn down by.
func TestModelCallHooks_AfterIsToldWhatTheCallUsed(t *testing.T) {
	var log []string

	reply := textResponse("done")
	reply.Usage = &responses.Usage{InputTokens: 120, OutputTokens: 34, TotalTokens: 154}

	llm := &scriptedLLM{script: []*responses.Response{reply}}
	hook := &budgetHook{name: "budget", log: &log}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Hooks: []agents.Hook{hook},
	}).WithLLM(llm)

	runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-usage", Message: userMessage("go"),
	})

	require.Len(t, hook.charged, 1)
	assert.Equal(t, 154, hook.charged[0].TotalTokens)
	assert.Equal(t, 120, hook.charged[0].InputTokens)
}

// A hook that answers for the model still runs the after phase, so a hook that
// records spend sees the call it refused — at zero.
func TestModelCallHooks_AfterRunsEvenWhenHandled(t *testing.T) {
	var log []string

	llm := &scriptedLLM{script: []*responses.Response{}}
	hook := &budgetHook{
		name: "budget",
		log:  &log,
		before: func(call *agents.ModelCall) (agents.ModelCallHookResult, error) {
			return agents.HandleModelCall(agents.ModelCallText("no credits")), nil
		},
	}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Hooks: []agents.Hook{hook},
	}).WithLLM(llm)

	runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-handled-after", Message: userMessage("go"),
	})

	assert.Equal(t, []string{"before:budget", "after:budget"}, log)
	require.Len(t, hook.charged, 1)
	assert.Equal(t, 0, hook.charged[0].TotalTokens, "a call never made costs nothing")
}

// The zero value carries on, and handling with no response is still handling.
func TestModelCallHookResult_ZeroValueContinues(t *testing.T) {
	assert.Equal(t, agents.ModelCallHookResult{}, agents.ContinueModelCall())
	assert.False(t, agents.ContinueModelCall().Handled)

	handled := agents.HandleModelCall(nil)
	assert.True(t, handled.Handled)
	assert.Nil(t, handled.Response)
}
