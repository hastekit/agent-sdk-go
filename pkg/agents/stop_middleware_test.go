package agents_test

import (
	"context"
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
			}
		})
	}
}
