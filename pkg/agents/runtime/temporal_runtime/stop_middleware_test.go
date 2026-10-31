package temporal_runtime_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hastekit/agent-sdk-go/internal/testutil"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/agents/prompts"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/temporal_runtime"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
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
	return func(ctx context.Context, call *agents.ModelCall, _ *responses.Request) (*responses.Response, error) {
		return nil, h.wait(ctx, call.StreamID)
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
				fn = temporal_runtime.NewTemporalMCPClient(testutil.NewMCPClient(&transformToolset{tool: tool}), broker, middleware).ExecuteTool
				args = []any{tool.BaseTool, call, map[string]any{}}
			case "model":
				fn = temporal_runtime.NewTemporalLLM(nil, broker, middleware).NewStreamingResponsesActivity
				args = []any{&responses.Request{}, &agents.ModelCall{StreamID: "model-stream"}}
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

// chunkProvider streams one message.
type chunkProvider struct{ llm.Provider }

func (chunkProvider) NewStreamingResponses(context.Context, *responses.Request) (chan *responses.ResponseChunk, error) {
	ch := make(chan *responses.ResponseChunk, 2)
	ch <- &responses.ResponseChunk{OfOutputItemDone: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{
		Item: responses.ChunkOutputItemData{Type: "message", Id: responses.NewOutputItemMessageID(),
			Content: &responses.ChunkOutputItemContent{{OfOutputText: &responses.OutputTextContent{Text: "hello"}}}},
	}}
	ch <- &responses.ResponseChunk{OfResponseCompleted: &responses.ChunkResponse[constants.ChunkTypeResponseCompleted]{}}
	close(ch)
	return ch, nil
}

// The model activity publishes chunks on the stream the call carries — the
// run's, which clients subscribe to — not on the workflow id, which is the
// run's execution and no client's channel.
func TestTemporalLLMActivityPublishesOnTheCallsStream(t *testing.T) {
	ctx := context.Background()
	broker := streambroker.NewMemoryStreamBroker()
	onStream, err := broker.Subscribe(ctx, "thread-stream")
	require.NoError(t, err)
	onWorkflow, err := broker.Subscribe(ctx, "default-test-workflow-id")
	require.NoError(t, err)

	fn := temporal_runtime.NewTemporalLLM(chunkProvider{}, broker).NewStreamingResponsesActivity
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(fn)
	_, err = env.ExecuteActivity(fn, &responses.Request{}, &agents.ModelCall{StreamID: "thread-stream"})
	require.NoError(t, err)

	require.NoError(t, broker.Close(ctx, "thread-stream"))
	require.NoError(t, broker.Close(ctx, "default-test-workflow-id"))
	var published, misplaced int
	for range onStream {
		published++
	}
	for range onWorkflow {
		misplaced++
	}
	require.Positive(t, published, "chunks reach the run's stream")
	require.Zero(t, misplaced, "nothing goes to the workflow id")
}

// A run started without a stream does not stream: the workflow id is the run's
// execution, not a channel, so nothing is published on it in the stream's
// place — as a local run without a stream publishes nothing.
func TestTemporalWorkflowWithoutAStreamPublishesNothing(t *testing.T) {
	ctx := context.Background()
	broker := streambroker.NewMemoryStreamBroker()
	const workflowID = "default-test-workflow-id"
	onWorkflow, err := broker.Subscribe(ctx, workflowID)
	require.NoError(t, err)

	a := temporal_runtime.NewTemporalAgent(nil, &agents.AgentOptions{
		Name: "quiet", LLM: chunkProvider{}, Instruction: prompts.New("You are quiet."),
		History: history.NewConversationManager(history.NewInMemoryConversationPersistence()),
	}, broker)
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	for name, fn := range a.GetActivities() {
		env.RegisterActivityWithOptions(fn, activity.RegisterOptions{Name: name})
	}
	for name, fn := range a.GetWorkflows() {
		env.RegisterWorkflowWithOptions(fn, workflow.RegisterOptions{Name: name})
	}
	env.ExecuteWorkflow("quiet_AgentWorkflow", &agents.AgentInput{
		Namespace: "tenant", ThreadID: "thread",
		Message: messages.New("user", []responses.InputMessageUnion{responses.UserMessage("hi")}),
	})
	require.NoError(t, env.GetWorkflowError())
	var out agents.AgentOutput
	require.NoError(t, env.GetWorkflowResult(&out))
	require.Equal(t, "hello", out.Text(), "the run itself is unaffected")

	require.NoError(t, broker.Close(ctx, workflowID))
	var misplaced int
	for range onWorkflow {
		misplaced++
	}
	require.Zero(t, misplaced, "nothing is published on the workflow id")
}
