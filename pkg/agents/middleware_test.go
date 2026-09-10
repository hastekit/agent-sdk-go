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

// bothMiddleware wraps tool calls and model calls, which one type is allowed to do.
type bothMiddleware struct {
	agents.NoopMiddleware

	name string
	log  *[]string
}

func (h *bothMiddleware) WrapToolCall(next agents.ToolCallFunc) agents.ToolCallFunc {
	return func(ctx context.Context, tool *agents.BaseTool, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
		*h.log = append(*h.log, "tool:wrap")
		return next(ctx, tool, call)
	}
}

func (h *bothMiddleware) WrapModelCall(next agents.ModelCallFunc) agents.ModelCallFunc {
	return func(ctx context.Context, call *agents.ModelCall, request *responses.Request) (*responses.Response, error) {
		*h.log = append(*h.log, "model:wrap")
		return next(ctx, call, request)
	}
}

// A middleware that only wraps one side embeds the no-op half for the other.
type toolOnlyMiddleware struct {
	agents.NoopMiddleware
	name string
	log  *[]string
}

func (h *toolOnlyMiddleware) WrapToolCall(next agents.ToolCallFunc) agents.ToolCallFunc {
	return func(ctx context.Context, tool *agents.BaseTool, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
		*h.log = append(*h.log, "tool:wrap")
		return next(ctx, tool, call)
	}
}

// Both sides of a middleware list are reachable, since a Middleware is both.
func TestMiddlewaresOf_NarrowBothWays(t *testing.T) {
	var log []string
	middlewares := []agents.Middleware{&bothMiddleware{name: "both", log: &log}, nil}

	assert.Len(t, agents.ToolCallMiddlewaresOf(middlewares), 1)
	assert.Len(t, agents.ModelCallMiddlewaresOf(middlewares), 1)
	assert.Nil(t, agents.ToolCallMiddlewaresOf(nil))
}

// The no-op half satisfies its side of the interface by wrapping with nothing:
// the function it returns is the one it was given.
func TestNoopHalves_SatisfyTheirSide(t *testing.T) {
	var log []string
	var _ agents.Middleware = &toolOnlyMiddleware{name: "tool-only", log: &log}

	modelCalled := false
	next := agents.ModelCallFunc(func(context.Context, *agents.ModelCall, *responses.Request) (*responses.Response, error) {
		modelCalled = true
		return &responses.Response{}, nil
	})
	_, err := agents.NoopMiddleware{}.WrapModelCall(next)(context.Background(), &agents.ModelCall{}, &responses.Request{})
	require.NoError(t, err)
	assert.True(t, modelCalled)

	toolCalled := false
	toolNext := agents.ToolCallFunc(func(context.Context, *agents.BaseTool, *agents.ToolCall) (*agents.ToolCallResponse, error) {
		toolCalled = true
		return nil, nil
	})
	_, err = agents.NoopMiddleware{}.WrapToolCall(toolNext)(context.Background(), &agents.BaseTool{}, &agents.ToolCall{})
	require.NoError(t, err)
	assert.True(t, toolCalled)
}

// One registration, both jobs: a middleware that implements both interfaces is
// consulted around the model call and around the tool call.
func TestMiddlewares_OneMiddlewareCanWrapBoth(t *testing.T) {
	var log []string

	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "worker", "{}"),
		textResponse("done"),
	}}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:        "main",
		Tools:       []agents.Tool{newFakeTool("worker", false, "tool ran")},
		Middlewares: []agents.Middleware{&bothMiddleware{name: "both", log: &log}},
	}).WithLLM(llm)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-both", Message: userMessage("go"),
	})

	requireStatus(t, out, agentstate.RunStatusCompleted)
	// First model call, then the tool it asked for, then the model call that
	// reads the result.
	assert.Equal(t, []string{"model:wrap", "tool:wrap", "model:wrap"}, log)
}
