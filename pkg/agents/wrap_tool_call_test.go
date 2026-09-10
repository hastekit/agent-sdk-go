package agents_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

// suffixMiddleware appends its name to whatever came back, and records when it ran.
type suffixMiddleware struct {
	agents.NoopMiddleware
	name string
	log  *[]string
	err  error
}

func (h *suffixMiddleware) WrapToolCall(next agents.ToolCallFunc) agents.ToolCallFunc {
	return func(ctx context.Context, tool *agents.BaseTool, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
		r, err := next(ctx, tool, call)
		if err != nil {
			return nil, err
		}
		*h.log = append(*h.log, "wrap:"+h.name)
		if h.err != nil {
			return nil, h.err
		}
		return agents.ToolCallResult(call, *r.Output.OfString+":"+h.name), nil
	}
}

// Middlewares nest with the first registered outermost, so a result flows from the
// tool through the last middleware's wrap to the first's.
func TestWrapToolCall_NestsFirstMiddlewareOutermost(t *testing.T) {
	var log []string
	call := &agents.ToolCall{FunctionCallMessage: &responses.FunctionCallMessage{ID: "id", CallID: "call", Name: "tool"}}
	out, err := agents.ExecuteToolWithMiddleware(t.Context(), []agents.ToolCallMiddleware{&suffixMiddleware{name: "a", log: &log}, &suffixMiddleware{name: "b", log: &log}}, agents.ExecutableToolCall{ToolCall: call}, func(context.Context, *agents.ToolCall) (*agents.ToolCallResponse, error) {
		log = append(log, "tool")
		return agents.ToolCallResult(call, "raw"), nil
	})
	require.NoError(t, err)
	require.Equal(t, "raw:b:a", *out.Output.OfString)
	require.Equal(t, []string{"tool", "wrap:b", "wrap:a"}, log)
}

// A middleware's own error ends the run, marked so the loop can tell.
func TestWrapToolCall_MiddlewareFailureAbortsTheCall(t *testing.T) {
	var log []string
	failure := errors.New("upload failed")
	call := &agents.ToolCall{FunctionCallMessage: &responses.FunctionCallMessage{}}
	out, err := agents.ExecuteToolWithMiddleware(t.Context(), []agents.ToolCallMiddleware{&suffixMiddleware{name: "a", log: &log, err: failure}}, agents.ExecutableToolCall{ToolCall: call}, func(context.Context, *agents.ToolCall) (*agents.ToolCallResponse, error) {
		return agents.ToolCallResult(call, "raw"), nil
	})
	require.Nil(t, out)
	require.ErrorIs(t, err, failure)
	require.True(t, agents.IsToolCallAborted(err))
	require.Equal(t, []string{"wrap:a"}, log)
}

// annotatingMiddleware passes the tool's error back with a note of its own.
type annotatingMiddleware struct {
	agents.NoopMiddleware
}

func (*annotatingMiddleware) WrapToolCall(next agents.ToolCallFunc) agents.ToolCallFunc {
	return func(ctx context.Context, tool *agents.BaseTool, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
		r, err := next(ctx, tool, call)
		if err != nil {
			return nil, fmt.Errorf("seen by annotating: %w", err)
		}
		return r, nil
	}
}

// The tool's own error passes back out through the wraps as the tool's: not
// marked as a middleware ending the run, and still the same error to errors.Is,
// whether a middleware passed it back untouched or with a note of its own.
func TestWrapToolCall_ToolFailureStaysTheTools(t *testing.T) {
	var log []string
	toolErr := errors.New("tool broke")
	call := &agents.ToolCall{FunctionCallMessage: &responses.FunctionCallMessage{}}
	fail := func(context.Context, *agents.ToolCall) (*agents.ToolCallResponse, error) { return nil, toolErr }

	_, err := agents.ExecuteToolCallWithMiddleware(t.Context(), []agents.ToolCallMiddleware{&suffixMiddleware{name: "a", log: &log}}, nil, call, fail)
	require.Same(t, toolErr, err, "passed back untouched, it comes out as the very error the tool returned")
	require.False(t, agents.IsToolCallAborted(err))

	_, err = agents.ExecuteToolCallWithMiddleware(t.Context(), []agents.ToolCallMiddleware{&annotatingMiddleware{}}, nil, call, fail)
	require.ErrorIs(t, err, toolErr)
	require.ErrorContains(t, err, "seen by annotating")
	require.False(t, agents.IsToolCallAborted(err))
}
