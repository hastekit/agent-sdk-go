package restate_runtime

import (
	"context"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

type stepWaitingMiddleware struct {
	agents.NoopMiddleware

	broker *streambroker.MemoryStreamBroker
}

func (h stepWaitingMiddleware) wait(ctx context.Context) error {
	_ = h.broker.Stop(ctx, "stream")
	<-ctx.Done()
	return ctx.Err()
}

func (h stepWaitingMiddleware) WrapToolCall(agents.ToolCallFunc) agents.ToolCallFunc {
	return func(ctx context.Context, _ *agents.BaseTool, _ *agents.ToolCall) (*agents.ToolCallResponse, error) {
		return nil, h.wait(ctx)
	}
}

func (h stepWaitingMiddleware) WrapModelCall(agents.ModelCallFunc) agents.ModelCallFunc {
	return func(ctx context.Context, _ *agents.ModelCall, _ *responses.Request) (*responses.Response, error) {
		return nil, h.wait(ctx)
	}
}

// Exercise the bodies passed to restate.Run. Their terminal cancellation is
// the outcome Restate records; the workflow-side executors do not watch stops.
func TestRestateStepMiddlewareCancellationIsTerminal(t *testing.T) {
	for _, kind := range []string{"tool", "mcp", "model"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			broker := streambroker.NewMemoryStreamBroker()
			middleware := stepWaitingMiddleware{broker: broker}
			tool := newBgTool("search")
			call := &agents.ToolCall{FunctionCallMessage: &responses.FunctionCallMessage{Name: "search"}, StreamID: "stream"}
			var err error
			switch kind {
			case "tool":
				_, err = NewRestateTool(nil, tool, broker, middleware).execute(ctx, call)
			case "mcp":
				_, err = NewRestateMCPTool(nil, nil, nil, *tool.BaseTool, broker, middleware).execute(ctx, call)
			case "model":
				step := NewRestateLLM(nil, nil, "", broker, "stream", middleware).(*RestateLLM)
				_, err = step.invoke(ctx, &agents.ModelCall{StreamID: "not-the-runtime-stream"}, &responses.Request{}, nil)
			}
			require.Error(t, err)
			require.True(t, wasCancelled(err), "cancellation must retain its terminal code across the journal")
			require.False(t, wasAborted(err))
		})
	}
}
