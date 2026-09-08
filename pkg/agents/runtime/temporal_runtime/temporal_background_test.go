package temporal_runtime_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/temporal_runtime"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// bgTool reports progress from its wait, then answers.
type bgTool struct {
	*agents.BaseTool
	err error
}

func newBgTool(name string) *bgTool {
	return &bgTool{BaseTool: &agents.BaseTool{
		ToolUnion: responses.ToolUnion{
			OfFunction: &responses.FunctionTool{
				Name:        name,
				Description: utils.Ptr("starts long work"),
				Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
			},
		},
	}}
}

func (t *bgTool) Execute(_ context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	return &agents.ToolCallResponse{
		FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			ID: params.ID, CallID: params.CallID,
			Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr("started")},
		},
		TaskID: "task-1",
	}, nil
}

func (t *bgTool) AwaitTask(ctx context.Context, ref agents.BackgroundTaskRef, progress agents.ProgressReporter) (agents.BackgroundResult, error) {
	progress.Report(ctx, agents.ToolProgress{Progress: 1, Total: 2, Message: "halfway"})
	if t.err != nil {
		return agents.BackgroundResult{}, t.err
	}
	return agents.BackgroundResult{Output: agents.BackgroundText("indexed 4210 documents")}, nil
}

// The loop asks the proxy, not the tool behind it, whether a task id is
// allowed. A proxy that dropped the capability would fail every run that
// started one.
func TestTemporalToolProxy_KeepsTheBackgroundCapability(t *testing.T) {
	// No workflow context needed: nothing here runs the tool, it only asks
	// what the tool is.
	background := temporal_runtime.NewTemporalToolProxy(nil, "agent_index", newBgTool("index"))
	plain := temporal_runtime.NewTemporalToolProxy(nil, "agent_plain", &plainTool{
		BaseTool: newBgTool("plain").BaseTool,
	})

	_, isBackground := background.(agents.BackgroundTool)
	assert.True(t, isBackground, "a tool that starts tasks stays one through the proxy")

	_, plainIsBackground := plain.(agents.BackgroundTool)
	assert.False(t, plainIsBackground, "an ordinary tool does not gain the capability")

	assert.Equal(t, "index", background.GetToolDescriptor().ToolUnion.OfFunction.Name,
		"the wrapper is a way of running the tool, not a different tool")
}

type plainTool struct{ *agents.BaseTool }

func (t *plainTool) Execute(_ context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	return &agents.ToolCallResponse{}, nil
}

// The wait runs as its own activity, and the progress it reports goes to the
// task's own stream — not the thread's, which belongs to whatever run holds it.
func TestTemporalAwaitTaskActivity_ReportsOnTheTaskStream(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()
	ref := agents.BackgroundTaskRef{
		TaskID: "task-1", CallID: "call_1", ToolName: "index",
		TaskStreamID:   "task-stream",
		ThreadStreamID: "thread-stream",
	}

	taskChunks, err := broker.Subscribe(context.Background(), ref.TaskStreamID)
	require.NoError(t, err)
	threadChunks, err := broker.Subscribe(context.Background(), ref.ThreadStreamID)
	require.NoError(t, err)

	task := temporal_runtime.NewTemporalBackgroundTask(newBgTool("index"), broker)

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivityWithOptions(task.AwaitTask, activity.RegisterOptions{Name: "AwaitTask"})

	encoded, err := env.ExecuteActivity("AwaitTask", ref)
	require.NoError(t, err)

	var result agents.BackgroundResult
	require.NoError(t, encoded.Get(&result))
	require.NotNil(t, result.Output)
	assert.Equal(t, "indexed 4210 documents", *result.Output.Output.OfString)

	require.NoError(t, broker.Close(context.Background(), ref.TaskStreamID))
	require.NoError(t, broker.Close(context.Background(), ref.ThreadStreamID))

	var onTask, onThread int
	for chunk := range taskChunks {
		if chunk.OfToolProgress != nil {
			onTask++
			assert.Equal(t, "call_1", chunk.OfToolProgress.CallID)
		}
	}
	for chunk := range threadChunks {
		if chunk.OfToolProgress != nil {
			onThread++
		}
	}

	assert.Equal(t, 1, onTask, "progress belongs to the task's own stream")
	assert.Zero(t, onThread, "and never to the thread's, which a later run resets")
}

// The wait's failure is data for the model, not a failure of the workflow: it
// still closes the stream and still delivers.
func TestTemporalAwaitTaskActivity_SurfacesAWaitFailure(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()
	tool := newBgTool("index")
	tool.err = errors.New("indexer unreachable")

	task := temporal_runtime.NewTemporalBackgroundTask(tool, broker)

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivityWithOptions(task.AwaitTask, activity.RegisterOptions{Name: "AwaitTask"})

	_, err := env.ExecuteActivity("AwaitTask", agents.BackgroundTaskRef{
		TaskID: "task-1", TaskStreamID: "task-stream", ThreadStreamID: "thread-stream",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "indexer unreachable")
}

// --- the background workflow ------------------------------------------------

const bgAgent = "main"

func bgInput() *temporal_runtime.BackgroundTaskInput {
	return &temporal_runtime.BackgroundTaskInput{
		AgentName: bgAgent,
		ToolName:  bgAgent + "_index",
		Ref: agents.BackgroundTaskRef{
			TaskID: "task-1", CallID: "call_1", ToolName: "index",
			Namespace: "test", ThreadID: "thread-1",
			TaskStreamID: "task-stream", ThreadStreamID: "thread-stream",
		},
	}
}

// The stream closes before the result is delivered, so a client watching the
// task sees it end when it ends rather than when the agent has replied.
func TestTemporalBackgroundWorkflow_ClosesTheStreamThenDelivers(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	var order []string
	registerBackgroundActivities(env, &order, &temporal_runtime.DeliverTaskOutput{Start: false})

	env.ExecuteWorkflow(temporal_runtime.NewBackgroundTaskWorkflow(bgAgent).Execute, bgInput())

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	assert.Equal(t, []string{"await", "close", "deliver"}, order)
}

// When the thread has gone idle, the task's workflow starts the run itself.
func TestTemporalBackgroundWorkflow_StartsARunWhenTheThreadIsIdle(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	var order []string
	registerBackgroundActivities(env, &order, &temporal_runtime.DeliverTaskOutput{
		Start:   true,
		Message: messages.New("", []responses.InputMessageUnion{responses.UserMessage("task-1 finished")}),
	})

	var ranWith *agents.AgentInput
	env.RegisterWorkflowWithOptions(
		func(ctx workflow.Context, in *agents.AgentInput) (*agents.AgentOutput, error) {
			ranWith = in
			return &agents.AgentOutput{}, nil
		},
		workflow.RegisterOptions{Name: bgAgent + "_AgentWorkflow"},
	)

	env.ExecuteWorkflow(temporal_runtime.NewBackgroundTaskWorkflow(bgAgent).Execute, bgInput())

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	require.NotNil(t, ranWith, "an idle thread is woken by the task's own workflow")
	assert.Equal(t, "thread-1", ranWith.ThreadID)
	assert.Equal(t, "thread-stream", ranWith.StreamID, "the run streams where the thread does")
}

// A result folded into a run already in flight must not start a second one.
func TestTemporalBackgroundWorkflow_DoesNotStartARunWhenOneIsLive(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	var order []string
	registerBackgroundActivities(env, &order, &temporal_runtime.DeliverTaskOutput{Start: false})

	started := false
	env.RegisterWorkflowWithOptions(
		func(ctx workflow.Context, in *agents.AgentInput) (*agents.AgentOutput, error) {
			started = true
			return &agents.AgentOutput{}, nil
		},
		workflow.RegisterOptions{Name: bgAgent + "_AgentWorkflow"},
	)

	env.ExecuteWorkflow(temporal_runtime.NewBackgroundTaskWorkflow(bgAgent).Execute, bgInput())

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	assert.False(t, started, "the live run drains the queue itself")
}

// A wait that failed still reaches the model: the failure travels to the
// delivery step as text rather than failing the workflow.
func TestTemporalBackgroundWorkflow_DeliversAFailedWait(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	env.RegisterActivityWithOptions(
		func(ctx context.Context, ref agents.BackgroundTaskRef) (agents.BackgroundResult, error) {
			return agents.BackgroundResult{}, errors.New("indexer unreachable")
		},
		activity.RegisterOptions{Name: bgAgent + "_index_AwaitTaskActivity"},
	)
	env.RegisterActivityWithOptions(
		func(ctx context.Context, ref agents.BackgroundTaskRef) error { return nil },
		activity.RegisterOptions{Name: bgAgent + "_CloseTaskStreamActivity"},
	)

	var delivered *temporal_runtime.DeliverTaskInput
	env.RegisterActivityWithOptions(
		func(ctx context.Context, in *temporal_runtime.DeliverTaskInput) (*temporal_runtime.DeliverTaskOutput, error) {
			delivered = in
			return &temporal_runtime.DeliverTaskOutput{Start: false}, nil
		},
		activity.RegisterOptions{Name: bgAgent + "_DeliverTaskActivity"},
	)

	env.ExecuteWorkflow(temporal_runtime.NewBackgroundTaskWorkflow(bgAgent).Execute, bgInput())

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError(), "a wait that failed is news, not a broken workflow")
	require.NotNil(t, delivered)
	assert.Contains(t, delivered.AwaitFail, "indexer unreachable")
}

func registerBackgroundActivities(env *testsuite.TestWorkflowEnvironment, order *[]string, decision *temporal_runtime.DeliverTaskOutput) {
	env.RegisterActivityWithOptions(
		func(ctx context.Context, ref agents.BackgroundTaskRef) (agents.BackgroundResult, error) {
			*order = append(*order, "await")
			return agents.BackgroundResult{Output: agents.BackgroundText("done")}, nil
		},
		activity.RegisterOptions{Name: bgAgent + "_index_AwaitTaskActivity"},
	)
	env.RegisterActivityWithOptions(
		func(ctx context.Context, ref agents.BackgroundTaskRef) error {
			*order = append(*order, "close")
			return nil
		},
		activity.RegisterOptions{Name: bgAgent + "_CloseTaskStreamActivity"},
	)
	env.RegisterActivityWithOptions(
		func(ctx context.Context, in *temporal_runtime.DeliverTaskInput) (*temporal_runtime.DeliverTaskOutput, error) {
			*order = append(*order, "deliver")
			return decision, nil
		},
		activity.RegisterOptions{Name: bgAgent + "_DeliverTaskActivity"},
	)
}
