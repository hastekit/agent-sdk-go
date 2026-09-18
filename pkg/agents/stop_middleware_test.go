package agents_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

type waitForStopMiddleware struct {
	agents.NoopMiddleware

	started chan struct{}
}

func (h waitForStopMiddleware) WrapToolCall(agents.ToolCallFunc) agents.ToolCallFunc {
	return func(ctx context.Context, _ *agents.BaseTool, _ *agents.ToolCall) (*agents.ToolCallResponse, error) {
		close(h.started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
}

func (h waitForStopMiddleware) WrapModelCall(agents.ModelCallFunc) agents.ModelCallFunc {
	return func(ctx context.Context, _ *agents.ModelCall, _ *responses.Request) (*responses.Response, error) {
		close(h.started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
}

func TestStopMiddlewareCancelsUserMiddleware(t *testing.T) {
	for _, kind := range []string{"tool", "model"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			broker := streambroker.NewMemoryStreamBroker()
			middleware := waitForStopMiddleware{started: make(chan struct{})}
			go func() {
				select {
				case <-middleware.started:
					_ = broker.Stop(ctx, "stream")
				case <-ctx.Done():
				}
			}()
			stop := agents.StopMiddleware{Watcher: broker}
			if kind == "tool" {
				_, err := agents.ExecuteToolCallWithMiddleware(ctx, []agents.ToolCallMiddleware{stop, middleware}, nil, &agents.ToolCall{StreamID: "stream"}, nil)
				require.ErrorIs(t, err, agents.ErrToolCancelled)
				require.False(t, agents.IsToolCallAborted(err))
			} else {
				_, err := agents.ExecuteModelCallWithMiddleware(ctx, []agents.ModelCallMiddleware{stop, middleware}, &agents.ModelCall{StreamID: "stream"}, nil, nil)
				require.ErrorIs(t, err, agents.ErrModelCallStopped)
				require.False(t, agents.IsModelCallAborted(err), "a stop is control flow, not a refusal")
			}
		})
	}
}

// failsOnStop is a provider that answers the stop's cancellation with an error
// of its own making, the way a client that does not wrap context.Canceled
// does.
type failsOnStop struct {
	started chan struct{}
}

func (p failsOnStop) call(ctx context.Context, _ *agents.ModelCall, _ *responses.Request) (*responses.Response, error) {
	close(p.started)
	<-ctx.Done()
	return nil, errors.New("stream closed by peer")
}

// A failure while the stop's cancellation is in force is the stop, whatever
// the provider made of the cancelled request. Reporting it as the provider's
// own would have a durable runtime retry the call the user just ended.
func TestStopMiddlewareReportsAnyFailureUnderAStopAsTheStop(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	broker := streambroker.NewMemoryStreamBroker()
	provider := failsOnStop{started: make(chan struct{})}
	go func() {
		select {
		case <-provider.started:
			_ = broker.Stop(ctx, "stream")
		case <-ctx.Done():
		}
	}()

	_, err := agents.ExecuteModelCallWithMiddleware(ctx, []agents.ModelCallMiddleware{agents.StopMiddleware{Watcher: broker}}, &agents.ModelCall{StreamID: "stream"}, nil, provider.call)

	require.ErrorIs(t, err, agents.ErrModelCallStopped)
	require.False(t, agents.IsModelCallAborted(err))
}

// With no stop in force, the provider's failure is the provider's, and comes
// back as it was.
func TestStopMiddlewareLeavesAnUnstoppedFailureAlone(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()
	boom := errors.New("503 from provider")

	_, err := agents.ExecuteModelCallWithMiddleware(t.Context(), []agents.ModelCallMiddleware{agents.StopMiddleware{Watcher: broker}}, &agents.ModelCall{StreamID: "stream"}, nil,
		func(context.Context, *agents.ModelCall, *responses.Request) (*responses.Response, error) {
			return nil, boom
		})

	require.Same(t, boom, err)
	require.False(t, agents.IsModelCallAborted(err))
}
