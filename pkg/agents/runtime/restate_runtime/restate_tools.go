package restate_runtime

import (
	"context"

	"github.com/hastekit/hastekit-sdk-go/pkg/agents"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
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

func (t *RestateTool) Tool(ctx context.Context) *responses.ToolUnion {
	return t.wrappedTool.Tool(ctx)
}

func (t *RestateTool) NeedApproval() bool {
	return t.wrappedTool.NeedApproval()
}

func (t *RestateTool) IsDeferred() bool {
	return t.wrappedTool.IsDeferred()
}

func (t *RestateTool) GetAnnotations() *agents.ToolAnnotations {
	return agents.AnnotationsOf(t.wrappedTool)
}
