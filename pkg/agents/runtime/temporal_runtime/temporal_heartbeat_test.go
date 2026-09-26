package temporal_runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// Observe retention independently of workflow events and normal activity execution.
type workerHeartbeatBroker struct {
	*streambroker.MemoryStreamBroker
	beats        atomic.Int32
	wrongChannel atomic.Bool
}

// StartHeartbeat is the only heartbeat method exposed by this test broker.
func (b *workerHeartbeatBroker) StartHeartbeat(ctx context.Context, channel string) func() {
	if channel != "explicit-stream" {
		b.wrongChannel.Store(true)
	}

	// Simulate broker-owned renewal without exposing its interval to the runtime.
	renewCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		b.beats.Add(1)
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-ticker.C:
				b.beats.Add(1)
			}
		}
	}()

	// Joining makes any renewed count stable once the activity exits.
	return func() {
		cancel()
		<-done
	}
}

// One scheduled heartbeat activity owns renewal until the workflow signals and joins it.
func TestTemporalHeartbeatActivityLifecycle(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "cancel", "retry", "child"} {
		t.Run(outcome, func(t *testing.T) {
			broker := &workerHeartbeatBroker{MemoryStreamBroker: streambroker.NewMemoryStreamBroker()}
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestWorkflowEnvironment()
			env.SetTestTimeout(10 * time.Second)

			// Exercise the real renewal loop; one failed attempt verifies Temporal retry recovery.
			var attempts, exits atomic.Int32
			var cancelMu sync.Mutex
			var cancelActivity context.CancelFunc
			// The SDK test environment completes cancellation immediately; deliver it to the real activity too.
			env.SetOnActivityCanceledListener(func(info *activity.Info) {
				if info.ActivityType.Name == "test"+heartbeatActivitySuffix {
					cancelMu.Lock()
					if cancelActivity != nil {
						cancelActivity()
					}
					cancelMu.Unlock()
				}
			})
			emitter := &streamHeartbeatActivity{broker: broker}
			env.RegisterActivityWithOptions(func(ctx context.Context, channel string) error {
				attempt := attempts.Add(1)
				defer exits.Add(1)
				if outcome == "retry" && attempt == 1 {
					return errors.New("worker failed")
				}
				activityCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				cancelMu.Lock()
				cancelActivity = cancel
				cancelMu.Unlock()
				return emitter.Run(activityCtx, channel)
			}, activity.RegisterOptions{Name: "test" + heartbeatActivitySuffix})

			// Ordinary work can be silent while the independent activity renews the stream.
			env.RegisterActivityWithOptions(func(ctx context.Context) error {
				ticker := time.NewTicker(time.Millisecond)
				defer ticker.Stop()
				for broker.beats.Load() < 3 {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-ticker.C:
					}
				}
				if outcome == "failure" {
					return errors.New("model failed")
				}
				return nil
			}, activity.RegisterOptions{Name: "quiet"})

			// Defer ordering matches ExecuteLocal: stop and join before closing the stream.
			var cleaned atomic.Bool
			run := func(ctx workflow.Context) error {
				stop := NewTemporalStreamBrokerProxy(ctx, "test", broker).StartHeartbeat(context.Background(), "explicit-stream")
				defer func() {
					stop()
					stop()
					cleaned.Store(true)
				}()
				ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
					StartToCloseTimeout: 5 * time.Second,
					RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
				})
				if err := workflow.ExecuteActivity(ctx, "quiet").Get(ctx, nil); err != nil {
					return err
				}
				// Renewal must also continue during a gap with no model/tool activity running.
				if outcome == "cancel" {
					return workflow.Sleep(ctx, time.Hour)
				}
				return workflow.Sleep(ctx, 30*time.Millisecond)
			}
			env.RegisterWorkflowWithOptions(run, workflow.RegisterOptions{Name: "run"})

			// Direct and child workflows use the same broker lifecycle, with an explicit stream ID unrelated to workflow IDs.
			if outcome == "cancel" {
				env.RegisterDelayedCallback(env.CancelWorkflow, 100*time.Millisecond)
			}
			if outcome == "child" {
				env.ExecuteWorkflow(func(ctx workflow.Context) error {
					return workflow.ExecuteChildWorkflow(ctx, "run").Get(ctx, nil)
				})
			} else {
				env.ExecuteWorkflow("run")
			}
			if outcome == "failure" || outcome == "cancel" {
				require.Error(t, env.GetWorkflowError())
			} else {
				require.NoError(t, env.GetWorkflowError())
			}

			// The workflow requests cancellation; join the real activity after the test harness synthesizes its result.
			require.True(t, cleaned.Load())
			require.Eventually(t, func() bool { return attempts.Load() == exits.Load() }, time.Second, time.Millisecond)
			wantAttempts := int32(1)
			if outcome == "retry" {
				wantAttempts = 2
			}
			require.Equal(t, wantAttempts, attempts.Load())
			require.False(t, broker.wrongChannel.Load())
			count := broker.beats.Load()
			require.GreaterOrEqual(t, count, int32(3))
			require.Never(t, func() bool { return broker.beats.Load() != count }, 20*time.Millisecond, time.Millisecond)
		})
	}
}

// Disabled streaming schedules no heartbeat activity.
func TestTemporalHeartbeatDisabledWithoutBroker(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.ExecuteWorkflow(func(ctx workflow.Context) error {
		stop := NewTemporalStreamBrokerProxy(ctx, "test", nil).StartHeartbeat(context.Background(), "s")
		stop()
		return nil
	})
	require.NoError(t, env.GetWorkflowError())
}
