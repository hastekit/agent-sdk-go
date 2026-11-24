package temporal_agent_builder

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentconfig"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/temporal_runtime"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/workflow"
)

const (
	backgroundWorkflowName  = WorkflowName + "_BackgroundTaskWorkflow"
	awaitTaskActivity       = WorkflowName + "_AwaitTaskActivity"
	closeTaskStreamActivity = WorkflowName + "_CloseTaskStreamActivity"
	deliverTaskActivity     = WorkflowName + "_DeliverTaskActivity"
)

// BackgroundTaskInput starts a task's background workflow: which catalog tool
// to ask about the task, and the snapshot a run started by its result enters,
// as the run that started the task saw it.
type BackgroundTaskInput struct {
	Snapshot *agentconfig.Snapshot    `json:"snapshot"`
	Agent    string                   `json:"agent"`
	Tool     string                   `json:"tool"`
	Ref      agents.BackgroundTaskRef `json:"ref"`
}

// AwaitTaskInput is the wait activity's input: the catalog key of the tool
// that started the task, the agent it ran for, and the task.
type AwaitTaskInput struct {
	Agent string                   `json:"agent"`
	Tool  string                   `json:"tool"`
	Ref   agents.BackgroundTaskRef `json:"ref"`
}

// AwaitTask waits for the task on the catalog's tool, the way a static agent's
// wait activity does, with the Builder's tool-call middlewares around it.
func (a *AgentBuilder) AwaitTask(ctx context.Context, in *AwaitTaskInput) (agents.BackgroundResult, error) {
	tool, err := a.builder.Tool(in.Tool)
	if err != nil {
		return agents.BackgroundResult{}, configError(err)
	}
	background, ok := tool.(agents.BackgroundTool)
	if !ok {
		return agents.BackgroundResult{}, configError(fmt.Errorf("%w: catalog tool %q starts no background tasks", agentconfig.ErrInvalid, in.Tool))
	}
	wrapped := agents.WrapBackgroundTool(in.Agent, background, agents.ToolCallMiddlewaresOf(a.builder.Middlewares())...)
	return temporal_runtime.NewTemporalBackgroundTask(wrapped, a.broker).AwaitTask(ctx, in.Ref)
}

// BackgroundTask waits for one task and delivers its outcome: into the run
// that holds the thread, or as a new run of the snapshot's agent. It is the
// config-built counterpart of temporal_runtime.BackgroundTaskWorkflow.
func (a *AgentBuilder) BackgroundTask(ctx workflow.Context, in *BackgroundTaskInput) error {
	if in == nil || in.Snapshot == nil {
		return configError(fmt.Errorf("%w: no background task input", agentconfig.ErrInvalid))
	}

	// The task owns retention for its own stream until its wait finishes.
	stopHeartbeat := temporal_runtime.NewTemporalStreamBrokerProxy(ctx, WorkflowName, a.broker).StartHeartbeat(context.Background(), in.Ref.TaskStreamID)
	defer stopHeartbeat()

	awaitCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: temporal_runtime.DefaultAwaitTaskTimeout})
	var result agents.BackgroundResult
	awaitErr := workflow.ExecuteActivity(awaitCtx, awaitTaskActivity, &AwaitTaskInput{Agent: in.Agent, Tool: in.Tool, Ref: in.Ref}).Get(awaitCtx, &result)

	shortCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: 30 * time.Second})

	// Stop renewal before closing, then close before delivering: a client
	// watching the task sees it end when it ends.
	stopHeartbeat()
	if err := workflow.ExecuteActivity(shortCtx, closeTaskStreamActivity, in.Ref).Get(shortCtx, nil); err != nil {
		return err
	}

	// The wait's failure is what the model is told about, so it travels as data.
	delivery := &temporal_runtime.DeliverTaskInput{Ref: in.Ref, Result: result}
	if awaitErr != nil {
		delivery.AwaitFail = awaitErr.Error()
	}
	var decision temporal_runtime.DeliverTaskOutput
	if err := workflow.ExecuteActivity(shortCtx, deliverTaskActivity, delivery).Get(shortCtx, &decision); err != nil {
		return err
	}
	if !decision.Start {
		return nil // folded into a run that is going, or queued for one about to start
	}

	// The thread was idle and this workflow now holds its claim: run the
	// snapshot's agent so it can react, as a child that outlives this one.
	runCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
		WorkflowID:        in.Ref.ThreadStreamID,
		ParentClosePolicy: enums.PARENT_CLOSE_POLICY_ABANDON,
	})
	run := workflow.ExecuteChildWorkflow(runCtx, WorkflowName, in.Snapshot, &agents.AgentInput{
		Namespace:   in.Ref.Namespace,
		ThreadID:    in.Ref.ThreadID,
		SessionID:   in.Ref.SessionID,
		StreamID:    in.Ref.ThreadStreamID,
		RunContext:  in.Ref.RunContext,
		Skills:      in.Ref.Skills,
		MCP:         in.Ref.MCP,
		ClientTools: in.Ref.ClientTools,
		Message:     decision.Message,
	})
	return run.GetChildWorkflowExecution().Get(runCtx, nil)
}

// backgroundRunner starts a task's wait as a child workflow that outlives the
// run which started it, carrying the run's snapshot so the result can start
// the same agent again.
type backgroundRunner struct {
	ctx      workflow.Context
	snapshot *agentconfig.Snapshot
}

var _ agents.BackgroundRunner = (*backgroundRunner)(nil)

func (r *backgroundRunner) StartTask(_ context.Context, tool agents.BackgroundTool, ref agents.BackgroundTaskRef) error {
	proxy, ok := tool.(*backgroundToolProxy)
	if !ok {
		return fmt.Errorf("background tool for task %s is not a config-built agent's catalog tool, so no activity can wait for it", ref.TaskID)
	}
	ctx := workflow.WithChildOptions(r.ctx, workflow.ChildWorkflowOptions{
		WorkflowID: backgroundWorkflowID(ref),
		// The run that started the task is about to finish and must not take
		// the wait down with it.
		ParentClosePolicy: enums.PARENT_CLOSE_POLICY_ABANDON,
		// The same task started twice is one task.
		WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
	})
	child := workflow.ExecuteChildWorkflow(ctx, backgroundWorkflowName, &BackgroundTaskInput{
		Snapshot: r.snapshot,
		Agent:    proxy.agent,
		Tool:     proxy.key,
		Ref:      ref,
	})
	// Wait for the child to have started, not finished: a parent that
	// completes first never starts it.
	return child.GetChildWorkflowExecution().Get(ctx, nil)
}

// backgroundWorkflowID keeps one execution per task. Namespaces choose agent
// names, so the namespace is part of it.
func backgroundWorkflowID(ref agents.BackgroundTaskRef) string {
	return "background-task/" + WorkflowName + "/" + url.PathEscape(ref.Namespace) + "/" + url.PathEscape(ref.AgentName) + "/" + url.PathEscape(ref.TaskID)
}
