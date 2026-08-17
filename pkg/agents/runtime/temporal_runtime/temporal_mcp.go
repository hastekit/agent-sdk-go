package temporal_runtime

import (
	"context"
	"fmt"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"go.temporal.io/sdk/workflow"
)

type TemporalMCPServer struct {
	wrappedMcpServer agents.MCPToolset
	broker           agents.StreamBroker
}

func NewTemporalMCPServer(wrappedMcpServer agents.MCPToolset, broker agents.StreamBroker) *TemporalMCPServer {
	return &TemporalMCPServer{
		wrappedMcpServer: wrappedMcpServer,
		broker:           broker,
	}
}

func (t *TemporalMCPServer) ListTools(ctx context.Context, runContext map[string]any) ([]agents.BaseTool, error) {
	mcpTools, err := t.wrappedMcpServer.ListTools(ctx, runContext)
	if err != nil {
		return nil, err
	}

	var tools []agents.BaseTool
	for _, tool := range mcpTools {
		tools = append(tools, agents.BaseTool{
			ToolUnion:        *tool.Tool(ctx),
			RequiresApproval: tool.NeedApproval(),
			Deferred:         tool.IsDeferred(),
			Annotations:      agents.AnnotationsOf(tool),
		})
	}

	return tools, nil
}

// ExecuteTool is the _ExecuteMCPToolActivity implementation. It runs inside a
// Temporal activity (exactly once per real call), so the execute_tool span
// opened here fires once and is replay-safe. callTool does the real work.
func (t *TemporalMCPServer) ExecuteTool(ctx context.Context, params *agents.ToolCall, runContext map[string]any) (*agents.ToolCallResponse, error) {
	injectProgressReporter(ctx, t.broker, params)

	resp, err := agents.RunStoppableTool(ctx, agents.StopWatcherFrom(t.broker), 0, params,
		func(callCtx context.Context, p *agents.ToolCall) (*agents.ToolCallResponse, error) {
			return agents.ExecuteWithTrace(callCtx, nil, p, func(innerCtx context.Context, ip *agents.ToolCall) (*agents.ToolCallResponse, error) {
				return t.callTool(innerCtx, ip, runContext)
			})
		})

	return resp, cancellationError(err)
}

func (t *TemporalMCPServer) callTool(ctx context.Context, params *agents.ToolCall, runContext map[string]any) (*agents.ToolCallResponse, error) {
	// Use CallToolDirect if the wrapped MCPToolset supports it (e.g. MCPClient),
	// which calls the tool directly via the connection pool without re-listing.
	type directCaller interface {
		CallToolDirect(ctx context.Context, runContext map[string]any, params *agents.ToolCall) (*agents.ToolCallResponse, error)
	}

	if dc, ok := t.wrappedMcpServer.(directCaller); ok {
		return dc.CallToolDirect(ctx, runContext, params)
	}

	// Fallback: ListTools uses schema cache so this is still efficient
	mcpTools, err := t.wrappedMcpServer.ListTools(ctx, runContext)
	if err != nil {
		return nil, err
	}

	for _, tool := range mcpTools {
		if td := tool.Tool(ctx); td != nil && td.OfFunction != nil && params.Name == td.OfFunction.Name {
			return tool.Execute(ctx, params)
		}
	}

	return nil, fmt.Errorf("no tool found with name %s", params.Name)
}

type TemporalMCPProxy struct {
	workflowCtx workflow.Context
	prefix      string
}

func NewTemporalMCPProxy(workflowCtx workflow.Context, prefix string) *TemporalMCPProxy {
	return &TemporalMCPProxy{
		workflowCtx: workflowCtx,
		prefix:      prefix,
	}
}

func (t *TemporalMCPProxy) GetName() string {
	return t.prefix
}

func (t *TemporalMCPProxy) ListTools(ctx context.Context, runContext map[string]any) ([]agents.Tool, error) {
	var toolDefs []agents.BaseTool
	err := workflow.ExecuteActivity(t.workflowCtx, t.prefix+"_ListMCPToolsActivity", runContext).Get(t.workflowCtx, &toolDefs)
	if err != nil {
		return nil, err
	}

	var toolList []agents.Tool
	for _, toolDef := range toolDefs {
		toolList = append(toolList, NewTemporalMCPToolProxy(t.workflowCtx, t.prefix, runContext, toolDef))
	}

	return toolList, nil
}

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
	err := workflow.ExecuteActivity(t.workflowCtx, t.prefix+"_ExecuteMCPToolActivity", params, t.runContext).Get(t.workflowCtx, &output)
	if err != nil {
		return nil, err
	}

	return output, nil
}
