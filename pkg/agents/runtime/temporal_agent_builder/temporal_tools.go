package temporal_agent_builder

import (
	"context"
	"errors"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/temporal_runtime"
	"go.temporal.io/sdk/workflow"
)

const executeToolActivity = WorkflowName + "_ExecuteToolActivity"

// ToolCallInput is the tool activity's input: the call, and the catalog key of
// the tool to run it with.
type ToolCallInput struct {
	Tool string           `json:"tool"`
	Call *agents.ToolCall `json:"call"`
}

// ExecuteTool runs the catalog's tool the way a static agent's tool activity
// does: stop-aware, reporting progress, with the Builder's tool-call
// middlewares wrapping it.
func (a *AgentBuilder) ExecuteTool(ctx context.Context, in *ToolCallInput) (*agents.ToolCallResponse, error) {
	tool, err := a.builder.Tool(in.Tool)
	if err != nil {
		return nil, configError(err)
	}
	return temporal_runtime.NewTemporalTool(tool, a.broker, agents.ToolCallMiddlewaresOf(a.builder.Middlewares())...).Execute(ctx, in.Call)
}

// toolProxy is a catalog tool in the workflow: described by the tool itself,
// run in an ExecuteTool activity carrying its catalog key.
type toolProxy struct {
	ctx   workflow.Context
	agent string
	key   string
	tool  agents.Tool
}

// newToolProxy wraps agent's catalog tool key for the workflow, keeping the
// background capability when the tool has it — see backgroundToolProxy.
func newToolProxy(ctx workflow.Context, agent, key string, tool agents.Tool) agents.Tool {
	proxy := &toolProxy{ctx: ctx, agent: agent, key: key, tool: tool}
	if _, ok := tool.(agents.BackgroundTool); ok {
		return &backgroundToolProxy{toolProxy: proxy}
	}
	return proxy
}

func (t *toolProxy) Execute(_ context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	var output *agents.ToolCallResponse
	err := workflow.ExecuteActivity(t.ctx, executeToolActivity, &ToolCallInput{Tool: t.key, Call: params}).Get(t.ctx, &output)
	if err != nil {
		return nil, err
	}
	return output, nil
}

// GetToolDescriptor reports the catalog tool's own identity, which is what the
// loop and middlewares read.
func (t *toolProxy) GetToolDescriptor() *agents.BaseTool {
	if t.tool == nil {
		return &agents.BaseTool{}
	}
	return t.tool.GetToolDescriptor()
}

// backgroundToolProxy is the proxy for a catalog tool that starts background
// tasks. The loop asks the proxy, not the tool, whether a task id is allowed,
// so the proxy has to be one too; the runner reads its catalog key to start
// the task's own workflow, whose AwaitTask activity does the waiting.
type backgroundToolProxy struct {
	*toolProxy
}

var _ agents.BackgroundTool = (*backgroundToolProxy)(nil)

// AwaitTask is never called in the workflow: the background workflow waits.
func (t *backgroundToolProxy) AwaitTask(context.Context, agents.BackgroundTaskRef, agents.ProgressReporter) (agents.BackgroundResult, error) {
	return agents.BackgroundResult{}, errors.New("a background task is waited on by its own workflow, not inside the run that started it")
}
