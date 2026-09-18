package agents_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/require"
)

// requestTransform rewrites the request on its way to the provider, on a
// copy, and records what it was shown.
type requestTransform struct {
	agents.NoopMiddleware

	name string
	log  *[]string

	// seen is how many input messages each wrap was shown.
	seen []int

	// answer, when set, is returned without calling next.
	answer *responses.Response
	// nilResult makes the wrap swallow the reply, which the runner rejects.
	nilResult bool
	err       error
}

func (h *requestTransform) WrapModelCall(next agents.ModelCallFunc) agents.ModelCallFunc {
	return func(ctx context.Context, call *agents.ModelCall, req *responses.Request) (*responses.Response, error) {
		*h.log = append(*h.log, "wrap:"+h.name)
		h.seen = append(h.seen, len(req.Input.OfInputMessageList))
		if h.err != nil {
			return nil, h.err
		}
		if h.answer != nil {
			return h.answer, nil
		}
		if h.nilResult {
			return nil, nil
		}
		// A replacement built on a copy, as the contract asks: the caller's
		// request has to come out as it went in.
		instructions := ""
		if req.Instructions != nil {
			instructions = *req.Instructions
		}
		out := *req
		out.Instructions = utils.Ptr(instructions + "+" + h.name)
		return next(ctx, call, &out)
	}
}

// Middlewares nest with the first registered outermost: it sees the request first,
// each inner middleware is shown what the outer ones handed down, and the request
// passed in is never the one the provider is handed changed.
func TestWrapModelCall_NestsFirstMiddlewareOutermost(t *testing.T) {
	var log []string
	a := &requestTransform{name: "a", log: &log}
	b := &requestTransform{name: "b", log: &log}
	original := &responses.Request{Instructions: utils.Ptr("base")}
	call := &agents.ModelCall{AgentName: "agent"}
	var sent *responses.Request
	provider := func(_ context.Context, _ *agents.ModelCall, req *responses.Request) (*responses.Response, error) {
		sent = req
		return &responses.Response{}, nil
	}

	_, err := agents.ExecuteModelCallWithMiddleware(t.Context(), []agents.ModelCallMiddleware{a, nil, b}, call, original, provider)
	require.NoError(t, err)
	require.Equal(t, []string{"wrap:a", "wrap:b"}, log)
	require.Equal(t, "base+a+b", *sent.Instructions)
	require.Equal(t, "base", *original.Instructions, "the request passed in is left as it was")
	require.NotSame(t, original, sent)

	// No middlewares: the provider is handed the request as it was, not a copy.
	_, err = agents.ExecuteModelCallWithMiddleware(t.Context(), nil, call, original, provider)
	require.NoError(t, err)
	require.Same(t, original, sent)

	// A failure stops the chain where it is: the inner middleware and the provider
	// are never reached.
	log, sent = nil, nil
	failure := errors.New("cannot resolve")
	_, err = agents.ExecuteModelCallWithMiddleware(t.Context(), []agents.ModelCallMiddleware{&requestTransform{name: "x", log: &log, err: failure}, b}, call, original, provider)
	require.ErrorIs(t, err, failure)
	require.Equal(t, []string{"wrap:x"}, log)
	require.Nil(t, sent)

	// A wrap that swallows the reply is reported, not dereferenced.
	_, err = agents.ExecuteModelCallWithMiddleware(t.Context(), []agents.ModelCallMiddleware{&requestTransform{name: "nothing", log: &log, nilResult: true}}, call, original, provider)
	require.ErrorContains(t, err, "model call returned no response")
}

// A middleware that answers for the model ends the chain there: the middlewares inside it
// and the provider are never reached.
func TestWrapModelCall_AnAnsweringMiddlewareSkipsTheProviderAndInnerMiddlewares(t *testing.T) {
	var log []string
	answer := &requestTransform{name: "budget", log: &log, answer: agents.ModelCallText("out of credit")}
	inner := &requestTransform{name: "inner", log: &log}

	resp, err := agents.ExecuteModelCallWithMiddleware(t.Context(), []agents.ModelCallMiddleware{answer, inner}, &agents.ModelCall{}, &responses.Request{},
		func(context.Context, *agents.ModelCall, *responses.Request) (*responses.Response, error) {
			t.Fatal("the provider must not be called")
			return nil, nil
		})
	require.NoError(t, err)
	require.Equal(t, "out of credit", (*resp.Output[0].OfOutputMessage.Content)[0].OfOutputText.Text)
	require.Equal(t, []string{"wrap:budget"}, log)
	require.Empty(t, inner.seen)
}
