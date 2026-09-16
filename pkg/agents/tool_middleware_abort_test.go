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

// abortingMiddleware returns an error either before it calls next or after, and
// records how far it got.
type abortingMiddleware struct {
	agents.NoopMiddleware

	name    string
	err     error
	before  bool
	after   bool
	entered *int
	exited  *int
}

func (h *abortingMiddleware) WrapToolCall(next agents.ToolCallFunc) agents.ToolCallFunc {
	return func(ctx context.Context, tool *agents.BaseTool, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
		if h.entered != nil {
			*h.entered++
		}
		if h.before {
			return nil, h.err
		}
		result, err := next(ctx, tool, call)
		if h.exited != nil {
			*h.exited++
		}
		if err != nil {
			return nil, err
		}
		if h.after {
			return nil, h.err
		}
		return result, nil
	}
}

func abortTestCall() *agents.ToolCall {
	return &agents.ToolCall{FunctionCallMessage: &responses.FunctionCallMessage{
		ID: "fc_1", CallID: "call_1", Name: "search", Arguments: "{}",
	}}
}

// abortTestExecution pairs the call with the tool it names, which is what the
// runner needs to show a middleware what it is being asked about.
func abortTestExecution() agents.ExecutableToolCall {
	return agents.ExecutableToolCall{
		ToolName: "search",
		Tool:     newFakeTool("search", false, "tool ran"),
		ToolCall: abortTestCall(),
	}
}

// An error from a middleware is a hard stop: nothing inside it runs, the tool never
// runs, and the error comes back out rather than becoming the call's result.
func TestMiddlewareError_BeforeTheToolStopsTheChain(t *testing.T) {
	denied := errors.New("tenant mismatch")
	innerEntered := 0

	middlewares := []agents.ToolCallMiddleware{
		&abortingMiddleware{name: "authz", err: denied, before: true},
		&abortingMiddleware{name: "inner", entered: &innerEntered},
	}

	ran := false
	resp, err := agents.ExecuteToolWithMiddleware(t.Context(), middlewares, abortTestExecution(),
		func(context.Context, *agents.ToolCall) (*agents.ToolCallResponse, error) {
			ran = true
			return agents.ToolCallResult(abortTestCall(), "tool ran"), nil
		})

	require.Error(t, err)
	assert.Nil(t, resp, "an aborted call has no result to report")
	assert.False(t, ran, "the tool must not run once a middleware has errored")
	assert.Zero(t, innerEntered, "the chain stopped, so no inner middleware ran")

	assert.True(t, agents.IsToolCallAborted(err), "the loop can tell this from a broken tool")
	assert.ErrorIs(t, err, denied, "the middleware's own error is still reachable")
	assert.Contains(t, err.Error(), "tenant mismatch")
}

// An error after the tool has run ends the run too, and the middlewares outside it
// see the error go past rather than a result.
func TestMiddlewareError_AfterTheToolStillEndsTheRun(t *testing.T) {
	withheld := errors.New("result failed egress check")
	outerExited := 0

	middlewares := []agents.ToolCallMiddleware{
		&abortingMiddleware{name: "outer", exited: &outerExited},
		&abortingMiddleware{name: "egress", err: withheld, after: true},
	}

	resp, err := agents.ExecuteToolWithMiddleware(t.Context(), middlewares, abortTestExecution(),
		func(context.Context, *agents.ToolCall) (*agents.ToolCallResponse, error) {
			return agents.ToolCallResult(abortTestCall(), "tool ran"), nil
		})

	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Equal(t, 1, outerExited, "the outer middleware saw the call come back, as an error")
	assert.True(t, agents.IsToolCallAborted(err))
	assert.ErrorIs(t, err, withheld)
}

// A refusal the model should read is a response, not an error. This is what a
// middleware returns instead when it wants the run to carry on.
func TestMiddlewareRefusal_GoesInTheResponse(t *testing.T) {
	middlewares := []agents.ToolCallMiddleware{&refusingMiddleware{reason: "not allowed for this user"}}

	resp, err := agents.ExecuteToolWithMiddleware(t.Context(), middlewares, abortTestExecution(),
		func(context.Context, *agents.ToolCall) (*agents.ToolCallResponse, error) {
			t.Fatal("the tool should not run when a middleware answers the call")
			return nil, nil
		})

	require.NoError(t, err, "a refusal does not fail the run")
	require.NotNil(t, resp)
	assert.Equal(t, "not allowed for this user", *resp.Output.OfString)
}

// refusingMiddleware answers the call itself rather than failing it.
type refusingMiddleware struct {
	agents.NoopMiddleware
	reason string
}

func (h *refusingMiddleware) WrapToolCall(agents.ToolCallFunc) agents.ToolCallFunc {
	return func(_ context.Context, _ *agents.BaseTool, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
		return agents.ToolCallResult(call, h.reason), nil
	}
}

// A tool that fails is not a middleware that refused: its error stays unmarked, so the
// loop goes on reporting it to the model to work around.
func TestToolErrorIsNotAnAbort(t *testing.T) {
	broken := errors.New("disk on fire")

	resp, err := agents.ExecuteToolWithMiddleware(t.Context(),
		[]agents.ToolCallMiddleware{&abortingMiddleware{name: "audit"}}, abortTestExecution(),
		func(context.Context, *agents.ToolCall) (*agents.ToolCallResponse, error) {
			return nil, broken
		})

	require.Error(t, err)
	assert.Nil(t, resp)
	assert.ErrorIs(t, err, broken)
	assert.False(t, agents.IsToolCallAborted(err), "a broken tool is not a middleware's refusal")
}

// End to end through the loop: the run fails and the caller gets the middleware's
// error, rather than the model being handed a tool result it would work around.
func TestAgentLoop_MiddlewareErrorFailsTheRun(t *testing.T) {
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
		Middlewares:  []agents.Middleware{&abortingMiddleware{name: "authz", err: denied, before: true}},
	}).WithLLM(llm)

	handle, err := agent.Execute(context.Background(), &agents.AgentInput{
		Namespace: "test",
		ThreadID:  "thread-middleware-abort",
		StreamID:  "middleware-abort-stream",
		Message:   userMessage("search for something"),
	})
	require.NoError(t, err)

	_, err = handle.Result()
	require.Error(t, err, "a middleware's error must fail the run")
	assert.True(t, agents.IsToolCallAborted(err))
	assert.ErrorIs(t, err, denied)
	assert.False(t, ran, "the tool never ran")
}
