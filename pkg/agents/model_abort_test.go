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

// modelWrap is a ModelCallMiddleware made of one function, for tests that
// need a wrap with a particular shape and nothing else.
type modelWrap func(next agents.ModelCallFunc) agents.ModelCallFunc

func (w modelWrap) WrapModelCall(next agents.ModelCallFunc) agents.ModelCallFunc { return w(next) }

func execModel(ctx context.Context, middlewares []agents.ModelCallMiddleware, invoke agents.ModelCallFunc) error {
	_, err := agents.ExecuteModelCallWithMiddleware(ctx, middlewares, &agents.ModelCall{}, &responses.Request{}, invoke)
	return err
}

func providerFails(err error) agents.ModelCallFunc {
	return func(context.Context, *agents.ModelCall, *responses.Request) (*responses.Response, error) {
		return nil, err
	}
}

// A middleware's own error is what a durable runtime must not retry: what it
// refused once it would refuse again. It comes back marked so the runtime can
// tell, with the middleware's error still underneath.
func TestModelCallMiddleware_OwnErrorIsMarkedAborted(t *testing.T) {
	denied := errors.New("attachment not found")
	refuse := modelWrap(func(agents.ModelCallFunc) agents.ModelCallFunc {
		return providerFails(denied)
	})
	reached := false
	err := execModel(t.Context(), []agents.ModelCallMiddleware{refuse}, func(context.Context, *agents.ModelCall, *responses.Request) (*responses.Response, error) {
		reached = true
		return nil, nil
	})

	require.True(t, agents.IsModelCallAborted(err))
	require.True(t, agents.IsTerminalModelCallError(err))
	require.ErrorIs(t, err, denied)
	require.False(t, reached, "a refusal never reaches the provider")
}

// The provider's error is the provider's: it comes back exactly as it was, so
// the runtime and any outer policy classify it on the provider's own terms.
func TestModelCallMiddleware_ProviderErrorComesBackAsItWas(t *testing.T) {
	boom := errors.New("503 from provider")
	passThrough := modelWrap(func(next agents.ModelCallFunc) agents.ModelCallFunc { return next })
	err := execModel(t.Context(), []agents.ModelCallMiddleware{passThrough}, providerFails(boom))

	require.Same(t, boom, err, "nothing added on the way out hands back the original")
	require.False(t, agents.IsModelCallAborted(err))
	require.False(t, agents.IsTerminalModelCallError(err))
}

// A middleware that says something about the provider's error keeps it the
// provider's: the wrapping is kept, and so is the identity underneath.
func TestModelCallMiddleware_WrappedProviderErrorStaysTheProviders(t *testing.T) {
	boom := errors.New("503 from provider")
	annotate := modelWrap(func(next agents.ModelCallFunc) agents.ModelCallFunc {
		return func(ctx context.Context, call *agents.ModelCall, request *responses.Request) (*responses.Response, error) {
			resp, err := next(ctx, call, request)
			if err != nil {
				return nil, fmt.Errorf("after %d loops: %w", call.LoopIteration, err)
			}
			return resp, nil
		}
	})
	err := execModel(t.Context(), []agents.ModelCallMiddleware{annotate}, providerFails(boom))

	require.ErrorIs(t, err, boom)
	require.Contains(t, err.Error(), "after 0 loops")
	require.False(t, agents.IsModelCallAborted(err))
}

// The caller going away is control flow, not a refusal, whichever layer
// reports it.
func TestModelCallMiddleware_CallerCancellationIsNotAnAbort(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	bail := modelWrap(func(agents.ModelCallFunc) agents.ModelCallFunc {
		return func(ctx context.Context, _ *agents.ModelCall, _ *responses.Request) (*responses.Response, error) {
			return nil, ctx.Err()
		}
	})
	err := execModel(ctx, []agents.ModelCallMiddleware{bail}, providerFails(errors.New("unreached")))

	require.ErrorIs(t, err, context.Canceled)
	require.False(t, agents.IsModelCallAborted(err))
}

// A wrap that returns neither a reply nor an error has made a mistake of its
// own, and it is reported as one rather than retried.
func TestModelCallMiddleware_NoReplyIsTheMiddlewaresMistake(t *testing.T) {
	silent := modelWrap(func(agents.ModelCallFunc) agents.ModelCallFunc {
		return func(context.Context, *agents.ModelCall, *responses.Request) (*responses.Response, error) {
			return nil, nil
		}
	})
	err := execModel(t.Context(), []agents.ModelCallMiddleware{silent}, providerFails(errors.New("unreached")))

	require.Error(t, err)
	require.True(t, agents.IsModelCallAborted(err))
}
