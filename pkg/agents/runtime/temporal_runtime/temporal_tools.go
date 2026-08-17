package temporal_runtime

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/workflow"
)

// injectProgressReporter re-establishes a tool call's progress sink inside an
// activity. ToolCall.Progress does not survive the workflow→activity
// serialization boundary, so the loop-side reporter is gone by the time the
// tool runs here; we rebuild a broker-backed one. The stream channel is the
// workflow execution id, which equals the run's StreamID (see
// TemporalAgentV2.Execute) and is the same channel the LLM activity publishes
// on. Progress is a best-effort side stream, so a duplicate update on activity
// retry is acceptable — clients dedupe by call id + sequence.
func injectProgressReporter(ctx context.Context, broker agents.StreamBroker, params *agents.ToolCall) {
	if broker == nil || params == nil {
		return
	}
	streamID := activity.GetInfo(ctx).WorkflowExecution.ID
	params.Progress = agents.NewStreamProgressReporter(broker, streamID, params.CallID, params.Name)
}

type TemporalTool struct {
	wrappedTool agents.Tool
	broker      agents.StreamBroker
}

func NewTemporalTool(wrappedTool agents.Tool, broker agents.StreamBroker) *TemporalTool {
	return &TemporalTool{
		wrappedTool: wrappedTool,
		broker:      broker,
	}
}

func (t *TemporalTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	injectProgressReporter(ctx, t.broker, params)

	resp, err := agents.RunStoppableTool(ctx, agents.StopWatcherFrom(t.broker), 0, params,
		func(callCtx context.Context, p *agents.ToolCall) (*agents.ToolCallResponse, error) {
			return agents.ExecuteWithTrace(callCtx, t.wrappedTool, p, t.wrappedTool.Execute)
		})

	return resp, cancellationError(err)
}

type TemporalToolProxy struct {
	workflowCtx workflow.Context
	prefix      string
	wrappedTool agents.Tool
}

func NewTemporalToolProxy(workflowCtx workflow.Context, prefix string, wrappedTool agents.Tool) agents.Tool {
	return &TemporalToolProxy{
		workflowCtx: workflowCtx,
		prefix:      prefix,
		wrappedTool: wrappedTool,
	}
}

func (t *TemporalToolProxy) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	var output *agents.ToolCallResponse
	err := workflow.ExecuteActivity(t.workflowCtx, t.prefix+"_ExecuteToolActivity", params).Get(t.workflowCtx, &output)
	if err != nil {
		return nil, err
	}

	return output, nil
}

func (t *TemporalToolProxy) Tool(ctx context.Context) *responses.ToolUnion {
	return t.wrappedTool.Tool(ctx)
}

func (t *TemporalToolProxy) NeedApproval() bool {
	return t.wrappedTool.NeedApproval()
}

func (t *TemporalToolProxy) IsDeferred() bool {
	return t.wrappedTool.IsDeferred()
}

func (t *TemporalToolProxy) GetAnnotations() *agents.ToolAnnotations {
	return agents.AnnotationsOf(t.wrappedTool)
}
