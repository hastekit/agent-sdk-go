package restate_runtime

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	restate "github.com/restatedev/sdk-go"
)

type RestateMCPServer struct {
	restateCtx       restate.WorkflowContext
	wrappedMcpServer agents.MCPToolset
	broker           agents.StreamBroker
}

func NewRestateMCPServer(restateCtx restate.WorkflowContext, wrappedMcpServer agents.MCPToolset, broker agents.StreamBroker) *RestateMCPServer {
	return &RestateMCPServer{
		restateCtx:       restateCtx,
		wrappedMcpServer: wrappedMcpServer,
		broker:           broker,
	}
}

func (t *RestateMCPServer) GetName() string {
	return ""
}

func (t *RestateMCPServer) ListTools(ctx context.Context, runContext map[string]any) ([]agents.Tool, error) {
	// ListTools uses the schema cache in MCPClient — no live connection needed on cache hit.
	toolDefs, err := restate.Run(t.restateCtx, func(ctx restate.RunContext) ([]agents.BaseTool, error) {
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
	}, restate.WithName("MCPListTools"))
	if err != nil {
		return nil, err
	}

	var tools []agents.Tool
	for _, tool := range toolDefs {
		tools = append(tools, NewRestateMCPTool(t.restateCtx, t.wrappedMcpServer, runContext, tool, t.broker))
	}

	return tools, nil
}

type RestateMCPTool struct {
	restateCtx       restate.WorkflowContext
	runContext       map[string]any
	wrappedMcpServer agents.MCPToolset
	broker           agents.StreamBroker
	*agents.BaseTool
}

func NewRestateMCPTool(restateCtx restate.WorkflowContext, wrappedMcpServer agents.MCPToolset, runContext map[string]any, baseTool agents.BaseTool, broker agents.StreamBroker) *RestateMCPTool {
	return &RestateMCPTool{
		restateCtx:       restateCtx,
		runContext:       runContext,
		wrappedMcpServer: wrappedMcpServer,
		broker:           broker,
		BaseTool:         &baseTool,
	}
}

// Execute runs the call inside a Restate run step, watching the stop flag
// so a stop cancels the MCP call rather than leaving the server working.
// The span runs inside the step, so execute_tool fires exactly once and
// never on replay. callTool does the work via the connection pool.
func (t *RestateMCPTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	return restate.Run(t.restateCtx, func(runCtx restate.RunContext) (*agents.ToolCallResponse, error) {
		resp, err := agents.RunStoppableTool(runCtx, agents.StopWatcherFrom(t.broker), 0, params,
			func(callCtx context.Context, p *agents.ToolCall) (*agents.ToolCallResponse, error) {
				return agents.ExecuteWithTrace(callCtx, t, p, t.callTool)
			})
		return resp, cancellationError(err)
	}, restate.WithName("MCPToolCall"))
}

// callTool invokes the MCP tool on the wrapped toolset.
func (t *RestateMCPTool) callTool(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	// Use CallToolDirect on the wrapped MCPToolset if it supports it,
	// otherwise fall back to ListTools + find (for non-MCPClient implementations).
	type directCaller interface {
		CallToolDirect(ctx context.Context, runContext map[string]any, params *agents.ToolCall) (*agents.ToolCallResponse, error)
	}

	if dc, ok := t.wrappedMcpServer.(directCaller); ok {
		return dc.CallToolDirect(ctx, t.runContext, params)
	}

	// Fallback: ListTools uses schema cache so this is still fast
	mcpTools, err := t.wrappedMcpServer.ListTools(ctx, t.runContext)
	if err != nil {
		return nil, err
	}
	for _, tool := range mcpTools {
		if td := tool.Tool(ctx); td != nil && td.OfFunction != nil && params.Name == td.OfFunction.Name {
			return tool.Execute(ctx, params)
		}
	}
	return nil, err
}
