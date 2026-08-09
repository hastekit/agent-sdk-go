package temporal_runtime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hastekit/hastekit-sdk-go/pkg/agents"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/runtime/temporal_runtime"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/hastekit-sdk-go/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// blockingTool runs until its context is cancelled — the tool the user
// gives up on.
type blockingTool struct {
	*agents.BaseTool
	started chan struct{}
}

func newBlockingTool() *blockingTool {
	return &blockingTool{
		BaseTool: &agents.BaseTool{
			ToolUnion: responses.ToolUnion{
				OfFunction: &responses.FunctionTool{
					Name:        "slow",
					Description: utils.Ptr("test tool"),
					Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
				},
			},
		},
		started: make(chan struct{}),
	}
}

func (t *blockingTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	close(t.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

func toolCall(streamID string) *agents.ToolCall {
	return &agents.ToolCall{
		FunctionCallMessage: &responses.FunctionCallMessage{
			ID:        "fc_slow",
			CallID:    "call_slow",
			Name:      "slow",
			Arguments: "{}",
		},
		StreamID: streamID,
	}
}

// The activity is where the tool runs, so it is where a stop must land:
// cancelling from the workflow ends the wait, not the work.
func TestTemporalToolActivity_StopCancelsTheRunningTool(t *testing.T) {
	const streamID = "temporal-activity-stop"

	broker := streambroker.NewMemoryStreamBroker()
	tool := newBlockingTool()

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivityWithOptions(
		temporal_runtime.NewTemporalTool(tool, broker).Execute,
		activity.RegisterOptions{Name: "slow_ExecuteToolActivity"},
	)

	go func() {
		<-tool.started
		_ = broker.Stop(context.Background(), streamID)
	}()

	_, err := env.ExecuteActivity("slow_ExecuteToolActivity", toolCall(streamID))
	require.Error(t, err, "a stopped tool call must fail the activity, not return a result")

	// So the workflow can tell a stop from a tool that broke.
	var appErr *temporal.ApplicationError
	require.True(t, errors.As(err, &appErr), "want an ApplicationError, got %T: %v", err, err)
	assert.Equal(t, temporal_runtime.ToolCancelledErrorType, appErr.Type())

	// No RetryPolicy is set, so the server default is unlimited attempts:
	// a retryable failure would re-run the tool the user just stopped.
	assert.True(t, appErr.NonRetryable(), "the cancellation must not be retried")
}

// A tool that never checks its context cannot be taken away, but it must
// not hold the activity open either.
func TestTemporalToolActivity_AbandonsToolThatIgnoresContext(t *testing.T) {
	const streamID = "temporal-activity-abandon"

	broker := streambroker.NewMemoryStreamBroker()
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)

	stubborn := &stubbornTool{BaseTool: newBlockingTool().BaseTool, started: started, release: release}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivityWithOptions(
		temporal_runtime.NewTemporalTool(stubborn, broker).Execute,
		activity.RegisterOptions{Name: "slow_ExecuteToolActivity"},
	)

	go func() {
		<-started
		_ = broker.Stop(context.Background(), streamID)
	}()

	done := make(chan error, 1)
	go func() {
		_, err := env.ExecuteActivity("slow_ExecuteToolActivity", toolCall(streamID))
		done <- err
	}()

	select {
	case err := <-done:
		var appErr *temporal.ApplicationError
		require.True(t, errors.As(err, &appErr))
		assert.Equal(t, temporal_runtime.ToolCancelledErrorType, appErr.Type())
	case <-time.After(30 * time.Second):
		t.Fatal("the activity never returned: a tool ignoring ctx held the stop open")
	}
}

// stubbornTool ignores its context entirely.
type stubbornTool struct {
	*agents.BaseTool
	started chan struct{}
	release chan struct{}
}

func (t *stubbornTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	close(t.started)
	<-t.release
	return nil, nil
}

// The workflow reads the cancellation off an ActivityError wrapping the
// activity's ApplicationError — a different shape from what the activity
// itself returns, so it needs its own test. Getting it wrong reports a
// stopped call to the model as a tool failure.
func TestTemporalToolExecutor_ReadsCancellationFromActivityFailure(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	env.RegisterActivityWithOptions(
		func(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
			return nil, temporal.NewNonRetryableApplicationError(
				"stopped", temporal_runtime.ToolCancelledErrorType, nil)
		},
		activity.RegisterOptions{Name: "slow_ExecuteToolActivity"},
	)
	env.RegisterWorkflow(cancelledCallWorkflow)
	env.ExecuteWorkflow(cancelledCallWorkflow)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var cancelled []bool
	require.NoError(t, env.GetWorkflowResult(&cancelled))

	// The first call is reported cancelled, and the second is skipped as
	// cancelled without ever running.
	require.Equal(t, []bool{true, true}, cancelled)
}

// cancelledCallWorkflow runs two calls through the executor and reports
// each one's Cancelled flag.
func cancelledCallWorkflow(ctx workflow.Context) ([]bool, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
	})

	proxy := temporal_runtime.NewTemporalToolProxy(ctx, "slow", newBlockingTool())
	executions := []agents.ExecutableToolCall{
		{Index: 0, ToolName: "slow", Tool: proxy, ToolCall: toolCall("s")},
		{Index: 1, ToolName: "slow", Tool: proxy, ToolCall: toolCall("s")},
	}

	results := temporal_runtime.NewTemporalToolExecutor(ctx).ExecuteAll(context.Background(), executions)

	flags := make([]bool, len(results))
	for i, r := range results {
		flags[i] = r.Cancelled
	}
	return flags, nil
}
