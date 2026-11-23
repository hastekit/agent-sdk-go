package temporal_runtime

import (
	"context"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"go.temporal.io/sdk/workflow"
)

// TemporalMCPToolProxy executes a discovered tool through the agent's MCP activity.
type TemporalMCPToolProxy struct {
	workflowCtx workflow.Context
	prefix      string
	runContext  map[string]any
	*agents.BaseTool
}

func NewTemporalMCPToolProxy(workflowCtx workflow.Context, prefix string, runContext map[string]any, baseTool agents.BaseTool) *TemporalMCPToolProxy {
	return &TemporalMCPToolProxy{
		workflowCtx: workflowCtx,
		prefix:      prefix,
		runContext:  runContext,
		BaseTool:    &baseTool,
	}
}

func (t *TemporalMCPToolProxy) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	var output *agents.ToolCallResponse
	// The tool goes with the call: the activity side would otherwise have to
	// find it by name, and the name it has is the model-facing one.
	err := workflow.ExecuteActivity(t.workflowCtx, t.prefix+"_ExecuteMCPToolActivity", t.BaseTool, params, t.runContext).Get(t.workflowCtx, &output)
	if err != nil {
		return nil, err
	}

	return output, nil
}
