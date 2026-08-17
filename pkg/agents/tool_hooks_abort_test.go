package agents_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// abortingHook returns an error from whichever phase it is pointed at, and
// records what ran.
type abortingHook struct {
	agents.NoopModelCallHook

	name     string
	err      error
	onBefore bool
	onAfter  bool
	befores  *int
	afters   *int
}

func (h *abortingHook) GetName() string { return h.name }

func (h *abortingHook) BeforeToolCall(ctx context.Context, call *agents.ToolCall) (agents.ToolCallHookResult, error) {
	if h.befores != nil {
		*h.befores++
	}
	if h.onBefore {
		return agents.ContinueToolCall(), h.err
	}
	return agents.ContinueToolCall(), nil
}

func (h *abortingHook) AfterToolCall(ctx context.Context, call *agents.ToolCall, result *agents.ToolCallResponse) (agents.ToolCallHookResult, error) {
	if h.afters != nil {
		*h.afters++
	}
	if h.onAfter {
		return agents.ContinueToolCall(), h.err
	}
	return agents.ContinueToolCall(), nil
}

func abortTestCall() *agents.ToolCall {
	return &agents.ToolCall{FunctionCallMessage: &responses.FunctionCallMessage{
		ID: "fc_1", CallID: "call_1", Name: "search", Arguments: "{}",
	}}
}

// An error from a hook is a hard stop: the chain stops where it is, no later
// hook runs, the tool never runs, and the error comes back out rather than
// becoming the call's result.
func TestHookError_BeforeStopsTheChain(t *testing.T) {
	denied := errors.New("tenant mismatch")
	laterBefores, laterAfters := 0, 0

	hooks := []agents.ToolCallHook{
		&abortingHook{name: "authz", err: denied, onBefore: true},
		&abortingHook{name: "later", befores: &laterBefores, afters: &laterAfters},
	}

	ran := false
	resp, err := agents.RunWithToolCallHooks(t.Context(), hooks, abortTestCall(),
		func(context.Context, *agents.ToolCall) (*agents.ToolCallResponse, error) {
			ran = true
			return agents.ToolCallResult(abortTestCall(), "tool ran"), nil
		})

	require.Error(t, err)
	assert.Nil(t, resp, "an aborted call has no result to report")
	assert.False(t, ran, "the tool must not run once a hook has errored")
	assert.Zero(t, laterBefores, "the chain stopped, so no later hook ran")
	assert.Zero(t, laterAfters)

	assert.True(t, agents.IsToolCallAborted(err), "the loop can tell this from a broken tool")
	assert.ErrorIs(t, err, denied, "the hook's own error is still reachable")
	assert.Contains(t, err.Error(), "tenant mismatch")
}

// An error from the after phase ends the run too, though the tool has already
// run by then.
func TestHookError_AfterStopsTheChain(t *testing.T) {
	withheld := errors.New("result failed egress check")
	laterAfters := 0

	hooks := []agents.ToolCallHook{
		&abortingHook{name: "egress", err: withheld, onAfter: true},
		&abortingHook{name: "later", afters: &laterAfters},
	}

	resp, err := agents.RunWithToolCallHooks(t.Context(), hooks, abortTestCall(),
		func(context.Context, *agents.ToolCall) (*agents.ToolCallResponse, error) {
			return agents.ToolCallResult(abortTestCall(), "tool ran"), nil
		})

	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Zero(t, laterAfters, "the chain stopped at the error")
	assert.True(t, agents.IsToolCallAborted(err))
	assert.ErrorIs(t, err, withheld)
}

// A refusal the model should read is a response, not an error. This is what a
// hook returns instead when it wants the run to carry on.
func TestHookRefusal_GoesInTheResponse(t *testing.T) {
	hooks := []agents.ToolCallHook{&refusingHook{name: "authz", reason: "not allowed for this user"}}

	resp, err := agents.RunWithToolCallHooks(t.Context(), hooks, abortTestCall(),
		func(context.Context, *agents.ToolCall) (*agents.ToolCallResponse, error) {
			t.Fatal("the tool should not run when a hook answers the call")
			return nil, nil
		})

	require.NoError(t, err, "a refusal does not fail the run")
	require.NotNil(t, resp)
	assert.Equal(t, "not allowed for this user", *resp.Output.OfString)
}

// refusingHook answers the call itself rather than failing it.
type refusingHook struct {
	agents.NoopModelCallHook

	name   string
	reason string
}

func (h *refusingHook) GetName() string { return h.name }

func (h *refusingHook) BeforeToolCall(ctx context.Context, call *agents.ToolCall) (agents.ToolCallHookResult, error) {
	return agents.HandleToolCall(agents.ToolCallResult(call, h.reason)), nil
}

func (h *refusingHook) AfterToolCall(ctx context.Context, call *agents.ToolCall, result *agents.ToolCallResponse) (agents.ToolCallHookResult, error) {
	return agents.ContinueToolCall(), nil
}

// A tool that fails is not a hook that refused: its error stays unmarked, so the
// loop goes on reporting it to the model to work around.
func TestToolErrorIsNotAnAbort(t *testing.T) {
	broken := errors.New("disk on fire")

	resp, err := agents.RunWithToolCallHooks(t.Context(),
		[]agents.ToolCallHook{&abortingHook{name: "audit"}}, abortTestCall(),
		func(context.Context, *agents.ToolCall) (*agents.ToolCallResponse, error) {
			return nil, broken
		})

	require.Error(t, err)
	assert.Nil(t, resp)
	assert.ErrorIs(t, err, broken)
	assert.False(t, agents.IsToolCallAborted(err), "a broken tool is not a hook's refusal")
}

// End to end through the loop: the run fails and the caller gets the hook's
// error, rather than the model being handed a tool result it would work around.
func TestAgentLoop_HookErrorFailsTheRun(t *testing.T) {
	denied := errors.New("tenant mismatch")
	broker := streambroker.NewMemoryStreamBroker()

	llm := &scriptedLLM{script: []*responses.Response{{
		Output: []responses.OutputMessageUnion{
			{OfFunctionCall: &responses.FunctionCallMessage{ID: "fc_1", CallID: "call_1", Name: "search", Arguments: "{}"}},
		},
	}}}

	tool := newFakeTool("search", false, "should never run")
	ran := false
	tool.execute = func(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
		ran = true
		return agents.ToolCallResult(params, "tool ran"), nil
	}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:         "main",
		StreamBroker: broker,
		Tools:        []agents.Tool{tool},
		Hooks:        []agents.Hook{&abortingHook{name: "authz", err: denied, onBefore: true}},
	}).WithLLM(llm)

	handle, err := agent.Execute(context.Background(), &agents.AgentInput{
		Namespace: "test",
		ThreadID:  "thread-hook-abort",
		StreamID:  "hook-abort-stream",
		Message:   userMessage("search for something"),
	})
	require.NoError(t, err)

	_, err = handle.Result()
	require.Error(t, err, "a hook's error must fail the run")
	assert.True(t, agents.IsToolCallAborted(err))
	assert.ErrorIs(t, err, denied)
	assert.False(t, ran, "the tool never ran")
}
