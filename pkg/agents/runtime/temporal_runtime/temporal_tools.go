package temporal_runtime

import (
	"context"
	"fmt"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
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

// NewTemporalToolProxy wraps a tool for the workflow, keeping whichever
// capabilities the loop will ask it about — see TemporalBackgroundToolProxy.
func NewTemporalToolProxy(workflowCtx workflow.Context, prefix string, wrappedTool agents.Tool) agents.Tool {
	proxy := &TemporalToolProxy{
		workflowCtx: workflowCtx,
		prefix:      prefix,
		wrappedTool: wrappedTool,
	}
	if _, ok := wrappedTool.(agents.BackgroundTool); ok {
		return &TemporalBackgroundToolProxy{TemporalToolProxy: proxy}
	}
	return proxy
}

func (t *TemporalToolProxy) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	var output *agents.ToolCallResponse
	err := workflow.ExecuteActivity(t.workflowCtx, t.prefix+"_ExecuteToolActivity", params).Get(t.workflowCtx, &output)
	if err != nil {
		return nil, err
	}

	return output, nil
}

// GetToolDescriptor reports the wrapped tool's own identity, not this
// wrapper's: the wrapper is a way of running the tool, not a different tool,
// and it is what the loop reads and what a hook is shown.
func (t *TemporalToolProxy) GetToolDescriptor() *agents.BaseTool {
	// Nil-safe because this one is asked on every tool call, by the hook runner,
	// where losing the tool's identity is a far better outcome than a panic.
	if t.wrappedTool == nil {
		return &agents.BaseTool{}
	}
	return t.wrappedTool.GetToolDescriptor()
}

// TemporalBackgroundToolProxy is the proxy for a tool that also starts
// background tasks. The loop asks a tool whether it is an agents.BackgroundTool
// before letting it answer with a task id, and it asks the proxy, not the tool
// behind it — so the proxy has to be one too.
//
// Its AwaitTask is never called in the workflow. Waiting is what the background
// workflow's activity does, on the real tool; this exists so the loop
// recognises the capability, and so the runner learns which activity waits.
type TemporalBackgroundToolProxy struct {
	*TemporalToolProxy
}

var (
	_ agents.BackgroundTool   = (*TemporalBackgroundToolProxy)(nil)
	_ backgroundActivityNamer = (*TemporalBackgroundToolProxy)(nil)
)

func (t *TemporalBackgroundToolProxy) AwaitTask(context.Context, agents.BackgroundTaskRef, agents.ProgressReporter) (agents.BackgroundResult, error) {
	return agents.BackgroundResult{}, fmt.Errorf(
		"a background task is waited on by its own workflow, not inside the run that started it")
}

// BackgroundActivityName is the activity registered to wait for this tool's
// tasks — the tool's activity prefix, which is scoped to the agent the same
// way every other activity here is.
func (t *TemporalBackgroundToolProxy) BackgroundActivityName() string { return t.prefix }
