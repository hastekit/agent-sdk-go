package temporal_runtime

import (
	"context"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

const heartbeatActivitySuffix = "_StreamHeartbeatActivity"

// startHeartbeatActivity schedules one activity and joins its cancellation before stream closure.
func startHeartbeatActivity(workflowCtx workflow.Context, prefix string, broker agents.StreamBroker, channel string) func() {
	// Disabled streaming has no heartbeat activity; every configured broker owns its lifecycle.
	if broker == nil || channel == "" {
		return func() {}
	}

	// Replaying older histories must not insert a new activity into their command sequence.
	if workflow.GetVersion(workflowCtx, "stream-heartbeat-activity", workflow.DefaultVersion, 1) == workflow.DefaultVersion {
		return func() {}
	}

	// Retry crashed attempts while the run remains active; no periodic workflow timers are needed.
	ctx, cancel := workflow.WithCancel(workflowCtx)
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 24 * time.Hour,
		HeartbeatTimeout:    5 * time.Second,
		WaitForCancellation: true,
		RetryPolicy:         &temporal.RetryPolicy{InitialInterval: time.Second, MaximumInterval: 5 * time.Second},
	})
	future := workflow.ExecuteActivity(ctx, prefix+heartbeatActivitySuffix, channel)

	// A disconnected context lets cleanup finish even when the workflow itself was cancelled.
	stopped := false
	return func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		cleanupCtx, _ := workflow.NewDisconnectedContext(workflowCtx)
		if err := future.Get(cleanupCtx, nil); err != nil && !temporal.IsCanceledError(err) {
			workflow.GetLogger(workflowCtx).Warn("stream heartbeat activity stopped with an error", "stream_id", channel, "error", err)
		}
	}
}

// streamHeartbeatActivity holds only worker dependencies; its input is the explicit stream ID.
type streamHeartbeatActivity struct {
	broker agents.StreamBroker
}

// Run starts broker-owned renewal and receives cancellation through Temporal heartbeats.
func (a *streamHeartbeatActivity) Run(ctx context.Context, channel string) error {
	// Record worker liveness before starting the broker's independent renewal loop.
	activity.RecordHeartbeat(ctx)
	stop := a.broker.StartHeartbeat(ctx, channel)
	defer stop()

	// Temporal heartbeats deliver cancellation and detect worker failure, independently of retention.
	liveness := time.NewTicker(time.Second)
	defer liveness.Stop()

	// Cancellation joins broker cleanup before Temporal acknowledges activity completion.
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-liveness.C:
			activity.RecordHeartbeat(ctx)
		}
	}
}
