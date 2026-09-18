package workflow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	tworkflow "go.temporal.io/sdk/workflow"
)

// TemporalExecutorOptions configures node activities and the walker. Activity
// defaults are a five-minute start-to-close timeout, ten-second heartbeat timeout,
// and one attempt. Explicit retry policies require idempotent node side effects.
type TemporalExecutorOptions struct {
	ActivityOptions tworkflow.ActivityOptions
	MaxSteps        int
}

// TemporalExecutor runs a graph as a Temporal workflow, dispatching nodes as
// activities. Dependencies stay on the worker; only node IDs and Input cross the
// activity boundary. Treat the compiled graph as immutable after construction.
// Conditional-edge routers run in workflow code and must be deterministic.
type TemporalExecutor struct {
	name    string
	graph   *Compiled
	options TemporalExecutorOptions
}

func NewTemporalExecutor(name string, graph *Compiled, options TemporalExecutorOptions) (*TemporalExecutor, error) {
	if strings.TrimSpace(name) == "" || graph == nil || len(graph.Nodes) == 0 {
		return nil, fmt.Errorf("workflow: Temporal executor requires a name and compiled graph")
	}
	a := &options.ActivityOptions
	if a.StartToCloseTimeout < 0 || a.ScheduleToCloseTimeout < 0 || a.ScheduleToStartTimeout < 0 || a.HeartbeatTimeout < 0 {
		return nil, fmt.Errorf("workflow: Temporal activity timeouts must not be negative")
	}
	if a.StartToCloseTimeout == 0 && a.ScheduleToCloseTimeout == 0 {
		a.StartToCloseTimeout = 5 * time.Minute
	}
	if a.HeartbeatTimeout == 0 {
		a.HeartbeatTimeout = 10 * time.Second
	}
	if a.RetryPolicy == nil {
		a.RetryPolicy = &temporal.RetryPolicy{MaximumAttempts: 1}
	} else {
		policy := *a.RetryPolicy
		policy.NonRetryableErrorTypes = append([]string(nil), policy.NonRetryableErrorTypes...)
		a.RetryPolicy = &policy
	}
	// Do not return shared state while cancelled activities are still unwinding.
	a.WaitForCancellation = true
	return &TemporalExecutor{name: name, graph: graph, options: options}, nil
}

// Register installs the workflow and its node activity on a worker before Start.
// The supplied name is the workflow type used with client.ExecuteWorkflow.
func (e *TemporalExecutor) Register(registry worker.Registry) {
	registry.RegisterWorkflowWithOptions(e.Execute, tworkflow.RegisterOptions{Name: e.name})
	registry.RegisterActivityWithOptions(e.executeNode, activity.RegisterOptions{Name: e.name + ".node"})
}

// Execute may also be called inside a host Temporal workflow. A human pause
// returns a checkpoint; resume by passing that Input with SetResume into a new
// execution, or by calling Execute again after a host workflow receives a signal.
// Like any Temporal workflow, an error result does not deliver its return value
// through client.WorkflowRun.Get; a host workflow can capture partial state here.
func (e *TemporalExecutor) Execute(ctx tworkflow.Context, in *Input) (*Input, error) {
	if in == nil {
		in = &Input{}
	}
	if in.RunID == "" {
		in.RunID = tworkflow.GetInfo(ctx).WorkflowExecution.ID
	}
	// The shared walker logs with slog; suppress it during replay. Activity logs
	// and Temporal history provide node-level execution diagnostics.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bound := &temporalNodeExecutor{ctx: ctx, owner: e}
	out, err := NewWalker(RuntimeOptions{MaxSteps: e.options.MaxSteps, Logger: logger}).Walk(
		temporalWalkerContext{ctx}, e.graph, in, bound,
	)
	if ctx.Err() != nil {
		return out, temporal.NewCanceledError()
	}
	return out, err
}

// This context is used only for the walker's synchronous cancellation checks.
// Temporal channels cannot be exposed as Go channels. Node code instead receives
// a real activity context with cancellation and heartbeats.
type temporalWalkerContext struct{ ctx tworkflow.Context }

func (c temporalWalkerContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c temporalWalkerContext) Done() <-chan struct{}       { return nil }
func (c temporalWalkerContext) Value(any) any               { return nil }
func (c temporalWalkerContext) Err() error {
	if c.ctx.Err() != nil {
		return context.Canceled
	}
	return nil
}

// The activity result deliberately excludes Go errors: pauses are successful
// activity results, while failures use Temporal's native error conversion.
type temporalNodeResult struct {
	Output map[string]any
	Port   string
	Pause  *PauseState
}

func (e *TemporalExecutor) executeNode(ctx context.Context, nodeID string, in *Input) (*temporalNodeResult, error) {
	node, ok := e.graph.Nodes[nodeID]
	if !ok {
		return nil, temporal.NewNonRetryableApplicationError("unknown workflow node: "+nodeID, "UnknownNode", nil)
	}
	// Heartbeats deliver server-side cancellation even to long-running API/MCP
	// calls. Stop the heartbeat goroutine before returning the activity result.
	done := make(chan struct{})
	stopped := make(chan struct{})
	interval := e.options.ActivityOptions.HeartbeatTimeout / 2
	if interval <= 0 {
		interval = time.Nanosecond
	}
	activity.RecordHeartbeat(ctx)
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				activity.RecordHeartbeat(ctx)
			}
		}
	}()
	defer func() { close(done); <-stopped }()
	result := runInvocation(ctx, Invocation{Node: node, NodeID: nodeID, Input: in}, func() {})
	if result.Err != nil {
		if errors.Is(result.Err, context.Canceled) {
			return nil, temporal.NewCanceledError()
		}
		return nil, result.Err
	}
	return &temporalNodeResult{Output: result.Output, Port: result.Port, Pause: result.Pause}, nil
}

type temporalNodeExecutor struct {
	ctx   tworkflow.Context
	owner *TemporalExecutor
}

var _ NodeExecutor = (*temporalNodeExecutor)(nil)

func (e *temporalNodeExecutor) ExecuteWave(_ context.Context, invs []Invocation) []Result {
	ctx, cancel := tworkflow.WithCancel(tworkflow.WithActivityOptions(e.ctx, e.owner.options.ActivityOptions))
	defer cancel()
	results := make([]Result, len(invs))
	selector := tworkflow.NewSelector(e.ctx)
	for i, inv := range invs {
		results[i].NodeID = inv.NodeID
		var future tworkflow.Future
		delayNode, isBuiltin := inv.Node.(*builtinNode)
		isDelay := isBuiltin && delayNode.delay != nil
		if isDelay {
			future = tworkflow.NewTimer(ctx, *delayNode.delay)
		} else {
			future = tworkflow.ExecuteActivity(ctx, e.owner.name+".node", inv.NodeID, inv.Input)
		}
		selector.AddFuture(future, func(f tworkflow.Future) {
			var value temporalNodeResult
			// Use a disconnected context to drain activity cancellations before
			// returning. Get is called only once this future is ready.
			drain, _ := tworkflow.NewDisconnectedContext(e.ctx)
			var err error
			if isDelay {
				err = f.Get(drain, nil)
				value.Port = DefaultPort
				value.Output = map[string]any{"nodes": map[string]any{inv.NodeID: map[string]any{"duration": delayNode.delay.String()}}}
			} else {
				err = f.Get(drain, &value)
			}
			if err == nil && ctx.Err() != nil {
				err = temporal.NewCanceledError()
			}
			if err != nil {
				if temporal.IsCanceledError(err) {
					err = fmt.Errorf("%w: %w", context.Canceled, err)
				}
				results[i].Err = err
				cancel()
				return
			}
			results[i].Output, results[i].Port, results[i].Pause = value.Output, value.Port, value.Pause
		})
	}
	// Selection on a disconnected context still observes futures cancelled by
	// the parent, but does not stop draining merely because the parent is cancelled.
	drain, _ := tworkflow.NewDisconnectedContext(e.ctx)
	for range invs {
		selector.Select(drain)
	}
	return results
}
