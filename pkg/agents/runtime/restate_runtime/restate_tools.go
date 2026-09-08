package restate_runtime

import (
	"context"
	"fmt"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	restate "github.com/restatedev/sdk-go"
)

type RestateTool struct {
	restateCtx  restate.WorkflowContext
	wrappedTool agents.Tool

	// broker is how the run step learns the run was stopped — the raw
	// broker, not the workflow-side proxy, since the watch runs inside
	// the step.
	broker agents.StreamBroker
}

func NewRestateTool(restateCtx restate.WorkflowContext, wrappedTool agents.Tool, broker agents.StreamBroker) *RestateTool {
	return &RestateTool{
		restateCtx:  restateCtx,
		wrappedTool: wrappedTool,
		broker:      broker,
	}
}

func (t *RestateTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	return restate.Run(t.restateCtx, func(runCtx restate.RunContext) (*agents.ToolCallResponse, error) {
		resp, err := agents.RunStoppableTool(runCtx, agents.StopWatcherFrom(t.broker), 0, params,
			func(callCtx context.Context, p *agents.ToolCall) (*agents.ToolCallResponse, error) {
				return agents.ExecuteWithTrace(callCtx, t.wrappedTool, p, t.wrappedTool.Execute)
			})
		return resp, cancellationError(err)
	}, restate.WithName(params.Name+"_ToolCall"))
}

// GetToolDescriptor reports the wrapped tool's own identity, not this
// wrapper's: the wrapper is a way of running the tool, not a different tool,
// and it is what the loop reads and what a hook is shown.
func (t *RestateTool) GetToolDescriptor() *agents.BaseTool {
	return t.wrappedTool.GetToolDescriptor()
}

// RestateBackgroundTool is the wrapper for a tool that also starts background
// tasks. The loop asks a tool whether it is an agents.BackgroundTool before
// letting it answer with a task id, and it asks this wrapper, not the tool
// behind it — so the wrapper has to be one too.
//
// Its AwaitTask is never called here. Waiting happens in the background
// service, on the real tool, in an invocation that outlives this run; this
// exists so the loop recognises the capability.
type RestateBackgroundTool struct {
	*RestateTool
}

var _ agents.BackgroundTool = (*RestateBackgroundTool)(nil)

func (t *RestateBackgroundTool) AwaitTask(context.Context, agents.BackgroundTaskRef, agents.ProgressReporter) (agents.BackgroundResult, error) {
	return agents.BackgroundResult{}, fmt.Errorf(
		"a background task is waited on by its own invocation, not inside the run that started it")
}

// newRestateTool wraps a tool for the workflow, keeping whichever capabilities
// the loop will ask it about.
func newRestateTool(restateCtx restate.WorkflowContext, wrappedTool agents.Tool, broker agents.StreamBroker) agents.Tool {
	tool := NewRestateTool(restateCtx, wrappedTool, broker)
	if _, ok := wrappedTool.(agents.BackgroundTool); ok {
		return &RestateBackgroundTool{RestateTool: tool}
	}
	return tool
}
