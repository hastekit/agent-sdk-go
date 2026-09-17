package workflow

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	tworkflow "go.temporal.io/sdk/workflow"
)

func TestTemporalExecutorPauseResumeAndDurableDelay(t *testing.T) {
	var calls atomic.Int32
	prepare := cancellationNode(func(context.Context, *Input) (map[string]any, string, error) {
		calls.Add(1)
		return map[string]any{"prepared": true}, DefaultPort, nil
	})
	delay, err := NewDelayNode("delay", DelayNodeConfig{Duration: 24 * time.Hour})
	require.NoError(t, err)
	human, err := NewHumanNode("human", HumanNodeConfig{Message: "Continue?"})
	require.NoError(t, err)
	graph, err := NewGraph("review").AddNode("prepare", prepare).AddNode("delay", delay).AddNode("human", human).
		AddEdge(StartNode, "prepare").AddEdge("prepare", "delay").AddEdge("delay", "human").
		AddEdgeOnPort("human", "approved", EndNode).AddEdgeOnPort("human", "rejected", EndNode).Compile()
	require.NoError(t, err)
	executor, err := NewTemporalExecutor("review", graph, TemporalExecutorOptions{})
	require.NoError(t, err)
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	executor.Register(env)
	started := env.Now()
	env.ExecuteWorkflow("review", &Input{})
	require.NoError(t, env.GetWorkflowError())
	var state Input
	require.NoError(t, env.GetWorkflowResult(&state))
	require.Equal(t, "human", state.Pause.NodeID)
	require.NotEmpty(t, state.RunID)
	require.Equal(t, NodeStatusCompleted, state.Status["delay"])
	require.GreaterOrEqual(t, env.Now().Sub(started), 24*time.Hour)
	require.EqualValues(t, 1, calls.Load())

	state.SetResume("human", map[string]any{"action": "approve"})
	resumed := suite.NewTestWorkflowEnvironment()
	executor.Register(resumed)
	resumed.ExecuteWorkflow("review", &state)
	require.NoError(t, resumed.GetWorkflowError())
	require.NoError(t, resumed.GetWorkflowResult(&state))
	// Decode into a fresh value so omitted JSON fields don't retain the prior pause.
	var completed Input
	require.NoError(t, resumed.GetWorkflowResult(&completed))
	require.Nil(t, completed.Pause)
	require.Equal(t, NodeStatusCompleted, completed.Status["human"])
	require.EqualValues(t, 1, calls.Load())
}

func TestTemporalExecutorRetryPolicy(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(map[bool]string{false: "default-no-retry", true: "explicit-retry"}[retry], func(t *testing.T) {
			var attempts atomic.Int32
			node := cancellationNode(func(context.Context, *Input) (map[string]any, string, error) {
				if attempts.Add(1) == 1 {
					return nil, "", errors.New("temporary failure")
				}
				return map[string]any{"ok": true}, DefaultPort, nil
			})
			graph, err := NewGraph("retry").AddNode("node", node).Compile()
			require.NoError(t, err)
			opts := TemporalExecutorOptions{}
			if retry {
				opts.ActivityOptions.RetryPolicy = &temporal.RetryPolicy{MaximumAttempts: 2, InitialInterval: time.Millisecond}
			}
			executor, err := NewTemporalExecutor("retry", graph, opts)
			require.NoError(t, err)
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestWorkflowEnvironment()
			executor.Register(env)
			env.ExecuteWorkflow("retry", &Input{})
			if retry {
				require.NoError(t, env.GetWorkflowError())
				require.EqualValues(t, 2, attempts.Load())
			} else {
				require.ErrorContains(t, env.GetWorkflowError(), "temporary failure")
				require.EqualValues(t, 1, attempts.Load())
			}
		})
	}
}

func TestTemporalExecutorFailureCancelsSiblingTimer(t *testing.T) {
	delay, err := NewDelayNode("delay", DelayNodeConfig{Duration: time.Hour})
	require.NoError(t, err)
	bad := cancellationNode(func(context.Context, *Input) (map[string]any, string, error) {
		return nil, "", errors.New("node failed")
	})
	graph, err := NewGraph("failure").AddNode("bad", bad).AddNode("delay", delay).Compile()
	require.NoError(t, err)
	executor, err := NewTemporalExecutor("failure", graph, TemporalExecutorOptions{})
	require.NoError(t, err)
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	executor.Register(env)
	env.RegisterWorkflowWithOptions(func(ctx tworkflow.Context) (*Input, error) {
		state, runErr := executor.Execute(ctx, nil)
		if runErr == nil {
			return nil, errors.New("expected failure")
		}
		return state, nil
	}, tworkflow.RegisterOptions{Name: "inspect"})
	env.ExecuteWorkflow("inspect")
	require.NoError(t, env.GetWorkflowError())
	var state Input
	require.NoError(t, env.GetWorkflowResult(&state))
	require.Equal(t, NodeStatusFailed, state.Status["bad"])
	require.Equal(t, NodeStatusCancelled, state.Status["delay"])
	require.Empty(t, state.RunContext)
}

func TestTemporalExecutorWorkflowCancellation(t *testing.T) {
	delay, err := NewDelayNode("delay", DelayNodeConfig{Duration: time.Hour})
	require.NoError(t, err)
	graph, err := NewGraph("cancel").AddNode("delay", delay).Compile()
	require.NoError(t, err)
	executor, err := NewTemporalExecutor("cancel", graph, TemporalExecutorOptions{})
	require.NoError(t, err)
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	executor.Register(env)
	env.RegisterDelayedCallback(env.CancelWorkflow, time.Second)
	env.ExecuteWorkflow("cancel", &Input{})
	require.True(t, temporal.IsCanceledError(env.GetWorkflowError()), "%v", env.GetWorkflowError())
}

func TestTemporalExecutorCancelsRunningActivity(t *testing.T) {
	var unwound atomic.Bool
	node := cancellationNode(func(ctx context.Context, _ *Input) (map[string]any, string, error) {
		<-ctx.Done()
		unwound.Store(true)
		return map[string]any{"late": true}, DefaultPort, nil
	})
	graph, err := NewGraph("cancel-activity").AddNode("node", node).Compile()
	require.NoError(t, err)
	executor, err := NewTemporalExecutor("cancel-activity", graph, TemporalExecutorOptions{
		ActivityOptions: tworkflow.ActivityOptions{HeartbeatTimeout: 20 * time.Millisecond},
	})
	require.NoError(t, err)
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.SetTestTimeout(5 * time.Second)
	executor.Register(env)
	env.RegisterDelayedCallback(env.CancelWorkflow, 100*time.Millisecond)
	env.ExecuteWorkflow("cancel-activity", &Input{})
	require.True(t, temporal.IsCanceledError(env.GetWorkflowError()), "%v", env.GetWorkflowError())
	// The SDK test harness resolves cancellation immediately; the running
	// activity observes it on its next heartbeat rather than through a server ack.
	require.Eventually(t, unwound.Load, time.Second, time.Millisecond, "activity must observe cancellation")
}

func TestTemporalExecutorMergesInGraphOrder(t *testing.T) {
	graph := NewGraph("parallel")
	for _, id := range []string{"a", "b"} {
		graph.AddNode(id, cancellationNode(func(context.Context, *Input) (map[string]any, string, error) {
			if id == "a" {
				time.Sleep(10 * time.Millisecond)
			}
			return map[string]any{"winner": id, id: true}, DefaultPort, nil
		}))
	}
	compiled, err := graph.Compile()
	require.NoError(t, err)
	executor, err := NewTemporalExecutor("parallel", compiled, TemporalExecutorOptions{})
	require.NoError(t, err)
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	executor.Register(env)
	env.ExecuteWorkflow("parallel", &Input{})
	require.NoError(t, env.GetWorkflowError())
	var state Input
	require.NoError(t, env.GetWorkflowResult(&state))
	require.Equal(t, map[string]any{"winner": "b", "a": true, "b": true}, state.RunContext)
}
