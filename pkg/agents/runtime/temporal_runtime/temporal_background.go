package temporal_runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/workflow"
)

const (
	awaitTaskActivitySuffix     = "_AwaitTaskActivity"
	closeTaskStreamActivityName = "_CloseTaskStreamActivity"
	deliverTaskActivityName     = "_DeliverTaskActivity"
	backgroundWorkflowSuffix    = "_BackgroundTaskWorkflow"
)

// DefaultAwaitTaskTimeout bounds how long a tool may wait for one background
// task. A background task is by nature slow — that is why it is one — so this
// is generous where an ordinary activity timeout is not.
const DefaultAwaitTaskTimeout = 24 * time.Hour

// BackgroundTaskInput is what the background workflow is started with: which
// tool to ask, and about what.
type BackgroundTaskInput struct {
	AgentName string                   `json:"agent_name"`
	ToolName  string                   `json:"tool_name"`
	Ref       agents.BackgroundTaskRef `json:"ref"`
}

// backgroundWorkflowID keeps a task's workflow to one execution. Two runs that
// answer with the same task id describe the same work, and Temporal rejects
// the duplicate rather than waiting on it twice.
func backgroundWorkflowID(agentName string, ref agents.BackgroundTaskRef) string {
	return "background-task/" + agentName + "/" + ref.TaskID
}

// TemporalBackgroundRunner starts a task's wait as a child workflow that
// outlives the run which started it.
//
// A goroutine cannot work here: the tool ran in an activity, and the activity
// is over. Nor can the agent workflow simply wait — the run is meant to carry
// on. A child workflow with ParentClosePolicy ABANDON is the shape that fits:
// the run starts it, stops caring, and completes, and the child keeps waiting
// on its own.
type TemporalBackgroundRunner struct {
	workflowCtx workflow.Context
	agentName   string
}

var _ agents.BackgroundRunner = (*TemporalBackgroundRunner)(nil)

func NewTemporalBackgroundRunner(ctx workflow.Context, agentName string) *TemporalBackgroundRunner {
	return &TemporalBackgroundRunner{workflowCtx: ctx, agentName: agentName}
}

func (r *TemporalBackgroundRunner) StartTask(_ context.Context, tool agents.BackgroundTool, ref agents.BackgroundTaskRef) error {
	namer, ok := tool.(backgroundActivityNamer)
	if !ok {
		return fmt.Errorf("background tool for task %s is not a workflow proxy, so no activity is registered to wait for it", ref.TaskID)
	}
	toolName := namer.BackgroundActivityName()

	ctx := workflow.WithChildOptions(r.workflowCtx, workflow.ChildWorkflowOptions{
		WorkflowID: backgroundWorkflowID(r.agentName, ref),
		// The run that started the task is about to finish and must not take
		// the wait down with it.
		ParentClosePolicy: enums.PARENT_CLOSE_POLICY_ABANDON,
		// The same task started twice is one task.
		WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
	})

	child := workflow.ExecuteChildWorkflow(ctx, r.agentName+backgroundWorkflowSuffix, &BackgroundTaskInput{
		AgentName: r.agentName,
		ToolName:  toolName,
		Ref:       ref,
	})

	// Wait for the child to have started, not to have finished. A parent that
	// completes before Temporal has recorded the child simply never starts it,
	// which is exactly the silent loss this whole path exists to avoid.
	return child.GetChildWorkflowExecution().Get(ctx, nil)
}

// BackgroundTaskWorkflow waits for one task and delivers its outcome.
//
// It is registered per agent, because the activity that waits is registered
// per tool per agent — the same scoping every other activity here uses.
type BackgroundTaskWorkflow struct {
	agentName string
}

func NewBackgroundTaskWorkflow(agentName string) *BackgroundTaskWorkflow {
	return &BackgroundTaskWorkflow{agentName: agentName}
}

func (w *BackgroundTaskWorkflow) Execute(ctx workflow.Context, in *BackgroundTaskInput) error {
	awaitCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: DefaultAwaitTaskTimeout,
	})

	var result agents.BackgroundResult
	awaitErr := workflow.ExecuteActivity(awaitCtx,
		in.ToolName+awaitTaskActivitySuffix, in.Ref).Get(awaitCtx, &result)

	shortCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
	})

	// Close first: a client watching the task sees it end when it ends, not
	// when the agent has finished reacting to it.
	if err := workflow.ExecuteActivity(shortCtx,
		w.agentName+closeTaskStreamActivityName, in.Ref).Get(shortCtx, nil); err != nil {
		return err
	}

	// The wait's failure is not this workflow's failure: it is what the model
	// is told about, so it travels as data.
	var awaitMessage string
	if awaitErr != nil {
		awaitMessage = awaitErr.Error()
	}

	var decision DeliverTaskOutput
	if err := workflow.ExecuteActivity(shortCtx, w.agentName+deliverTaskActivityName, &DeliverTaskInput{
		Ref:       in.Ref,
		Result:    result,
		AwaitFail: awaitMessage,
	}).Get(shortCtx, &decision); err != nil {
		return err
	}

	if !decision.Start {
		// Folded into a run that is already going, or onto the queue of one
		// about to start. Either way somebody else reads it.
		return nil
	}

	// The thread was idle, and this workflow now holds its claim. Run the
	// agent so it can react — as a child, so the wait is not holding a worker
	// slot while the model works.
	runCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
		WorkflowID:        in.Ref.ThreadStreamID,
		ParentClosePolicy: enums.PARENT_CLOSE_POLICY_ABANDON,
	})
	run := workflow.ExecuteChildWorkflow(runCtx, in.AgentName+"_AgentWorkflow", &agents.AgentInput{
		Namespace:  in.Ref.Namespace,
		ThreadID:   in.Ref.ThreadID,
		StreamID:   in.Ref.ThreadStreamID,
		RunContext: in.Ref.RunContext,
		Message:    decision.Message,
	})
	return run.GetChildWorkflowExecution().Get(runCtx, nil)
}

// DeliverTaskInput is what the delivery activity decides on.
type DeliverTaskInput struct {
	Ref    agents.BackgroundTaskRef `json:"ref"`
	Result agents.BackgroundResult  `json:"result"`

	// AwaitFail is the wait's own error, if it had one. It travels as text
	// because it is destined for the model, not for Temporal's retry logic.
	AwaitFail string `json:"await_fail,omitempty"`
}

// DeliverTaskOutput says whether the caller must now start a run.
type DeliverTaskOutput struct {
	Start   bool            `json:"start"`
	Message history.Message `json:"message"`
}

// TemporalBackgroundTask is the activity side: the real tool and the real
// broker, neither of which can cross into a workflow.
type TemporalBackgroundTask struct {
	tool   agents.BackgroundTool
	broker agents.StreamBroker
}

func NewTemporalBackgroundTask(tool agents.BackgroundTool, broker agents.StreamBroker) *TemporalBackgroundTask {
	return &TemporalBackgroundTask{tool: tool, broker: broker}
}

// AwaitTask runs the tool's wait. It is a long activity by design; progress
// the tool reports goes straight to the broker, on the task's own stream.
func (t *TemporalBackgroundTask) AwaitTask(ctx context.Context, ref agents.BackgroundTaskRef) (agents.BackgroundResult, error) {
	progress := agents.NewStreamProgressReporter(t.broker, ref.TaskStreamID, ref.CallID, ref.ToolName)
	return t.tool.AwaitTask(ctx, ref, progress)
}

// TemporalBackgroundDelivery holds the broker for the two activities that
// finish a task off.
type TemporalBackgroundDelivery struct {
	broker agents.StreamBroker
}

func NewTemporalBackgroundDelivery(broker agents.StreamBroker) *TemporalBackgroundDelivery {
	return &TemporalBackgroundDelivery{broker: broker}
}

func (d *TemporalBackgroundDelivery) CloseTaskStream(ctx context.Context, ref agents.BackgroundTaskRef) error {
	return agents.CloseBackgroundTaskStream(ctx, d.broker, ref)
}

func (d *TemporalBackgroundDelivery) DeliverTask(ctx context.Context, in *DeliverTaskInput) (*DeliverTaskOutput, error) {
	var awaitErr error
	if in.AwaitFail != "" {
		awaitErr = fmt.Errorf("%s", in.AwaitFail)
	}

	start, msg, err := agents.DeliverBackgroundResult(ctx, d.broker, in.Ref, in.Result, awaitErr)
	if err != nil {
		return nil, err
	}
	return &DeliverTaskOutput{Start: start, Message: msg}, nil
}

// backgroundActivityNamer is how the runner learns which activity waits for a
// tool's tasks. The proxy knows, because it knows what it was registered as;
// the tool behind it does not.
type backgroundActivityNamer interface {
	BackgroundActivityName() string
}
