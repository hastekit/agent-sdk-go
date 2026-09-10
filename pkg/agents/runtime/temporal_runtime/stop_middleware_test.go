package temporal_runtime_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/temporal_runtime"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

type activityStopWatcher struct {
	*streambroker.MemoryStreamBroker
	t *testing.T
}

func (b *activityStopWatcher) WatchStop(ctx context.Context, streamID string) (<-chan struct{}, func()) {
	assert.True(b.t, activity.IsActivity(ctx), "stop watching must run inside the activity")
	return b.MemoryStreamBroker.WatchStop(ctx, streamID)
}

type activityWaitingMiddleware struct {
	agents.NoopMiddleware

	broker *activityStopWatcher
}

func (h activityWaitingMiddleware) wait(ctx context.Context, streamID string) error {
	_ = h.broker.Stop(ctx, streamID)
	<-ctx.Done()
	return ctx.Err()
}

func (h activityWaitingMiddleware) WrapToolCall(agents.ToolCallFunc) agents.ToolCallFunc {
	return func(ctx context.Context, _ *agents.BaseTool, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
		return nil, h.wait(ctx, call.StreamID)
	}
}

func (h activityWaitingMiddleware) WrapModelCall(agents.ModelCallFunc) agents.ModelCallFunc {
	return func(ctx context.Context, _ *agents.ModelCall, _ *responses.Request) (*responses.Response, error) {
		return nil, h.wait(ctx, activity.GetInfo(ctx).WorkflowExecution.ID)
	}
}

func TestStopMiddlewareRunsInsideTemporalActivities(t *testing.T) {
	for _, kind := range []string{"tool", "mcp", "model"} {
		t.Run(kind, func(t *testing.T) {
			broker := &activityStopWatcher{MemoryStreamBroker: streambroker.NewMemoryStreamBroker(), t: t}
			middleware := activityWaitingMiddleware{broker: broker}
			tool := newMiddlewareTestTool("search")
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestActivityEnvironment()
			var fn any
			call := middlewareCall()
			call.StreamID = "tool-stream"
			args := []any{call}
			switch kind {
			case "tool":
				fn = temporal_runtime.NewTemporalTool(tool, broker, middleware).Execute
			case "mcp":
				fn = temporal_runtime.NewTemporalMCPServer(&transformToolset{tool: tool}, broker, middleware).ExecuteTool
				args = []any{tool.BaseTool, call, map[string]any{}}
			case "model":
				fn = temporal_runtime.NewTemporalLLM(nil, broker, middleware).NewStreamingResponsesActivity
				args = []any{&responses.Request{}, &agents.ModelCall{StreamID: "not-the-workflow-stream"}}
			}
			env.RegisterActivity(fn)
			_, err := env.ExecuteActivity(fn, args...)
			require.Error(t, err)
			require.True(t, temporal_runtime.WasStopped(err))
			require.False(t, temporal_runtime.WasAborted(err), "stopping middleware is not a policy refusal")
			var appErr *temporal.ApplicationError
			require.True(t, errors.As(err, &appErr))
			require.True(t, appErr.NonRetryable(), "Temporal must not restart the cancelled middleware chain")
			require.False(t, tool.ran)
		})
	}
}
