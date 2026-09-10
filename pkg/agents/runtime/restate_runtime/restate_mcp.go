package restate_runtime

import (
	"context"
	"errors"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	restate "github.com/restatedev/sdk-go"
)

type RestateMCPServer struct {
	middlewares      []agents.ToolCallMiddleware
	restateCtx       restate.WorkflowContext
	wrappedMcpServer agents.MCPToolset
	broker           agents.StreamBroker
}

func NewRestateMCPServer(restateCtx restate.WorkflowContext, wrappedMcpServer agents.MCPToolset, broker agents.StreamBroker, middlewares ...agents.ToolCallMiddleware) *RestateMCPServer {
	return &RestateMCPServer{
		restateCtx:       restateCtx,
		wrappedMcpServer: wrappedMcpServer,
		broker:           broker,
		middlewares:      middlewares,
	}
}

func (t *RestateMCPServer) GetName() string {
	return t.wrappedMcpServer.GetName()
}

func (t *RestateMCPServer) ListTools(ctx context.Context, runContext map[string]any) ([]agents.Tool, error) {
	// ListTools uses the schema cache in MCPClient — no live connection needed on cache hit.
	toolDefs, err := restate.Run(t.restateCtx, func(ctx restate.RunContext) ([]agents.BaseTool, error) {
		mcpTools, err := t.wrappedMcpServer.ListTools(ctx, runContext)
		if err != nil {
			return nil, toolsetListError(err)
		}

		// GetToolDescriptor rather than a field-by-field copy: this is the only
		// crossing a tool makes into the workflow, and anything left out here
		// is gone for good on the far side — the tool's own name and its meta
		// included, which is exactly what a middleware over there is looking at.
		var tools []agents.BaseTool
		for _, tool := range mcpTools {
			if encoded := tool.GetToolDescriptor(); encoded != nil {
				tools = append(tools, *encoded)
			}
		}

		return tools, nil
	}, restate.WithName("MCPListTools"))
	if err != nil {
		return nil, toolsetListErrorFrom(err)
	}

	var tools []agents.Tool
	for _, tool := range toolDefs {
		tools = append(tools, NewRestateMCPTool(t.restateCtx, t.wrappedMcpServer, runContext, tool, t.broker, t.middlewares...))
	}

	return tools, nil
}

type RestateMCPTool struct {
	middlewares      []agents.ToolCallMiddleware
	restateCtx       restate.WorkflowContext
	runContext       map[string]any
	wrappedMcpServer agents.MCPToolset
	*agents.BaseTool
}

func NewRestateMCPTool(restateCtx restate.WorkflowContext, wrappedMcpServer agents.MCPToolset, runContext map[string]any, baseTool agents.BaseTool, broker agents.StreamBroker, middlewares ...agents.ToolCallMiddleware) *RestateMCPTool {
	return &RestateMCPTool{
		restateCtx:       restateCtx,
		runContext:       runContext,
		wrappedMcpServer: wrappedMcpServer,
		middlewares:      append([]agents.ToolCallMiddleware{agents.StopMiddleware{Watcher: agents.StopWatcherFrom(broker)}}, middlewares...),
		BaseTool:         &baseTool,
	}
}

// Execute runs the call inside a Restate run step, watching the stop flag
// so a stop cancels the MCP call rather than leaving the server working.
// The span runs inside the step, so execute_tool fires exactly once and
// never on replay. callTool does the work via the connection pool.
func (t *RestateMCPTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	return restate.Run(t.restateCtx, func(runCtx restate.RunContext) (*agents.ToolCallResponse, error) {
		return t.execute(runCtx, params)
	}, restate.WithName("MCPToolCall"))
}

func (t *RestateMCPTool) execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	resp, err := agents.ExecuteWithTrace(ctx, t, params, func(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
		return agents.ExecuteToolCallWithMiddleware(ctx, t.middlewares, t.BaseTool, call, t.callTool)
	})

	return resp, cancellationError(err)
}

// callTool invokes the MCP tool on the wrapped toolset.
func (t *RestateMCPTool) callTool(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	// Use CallToolDirect on the wrapped MCPToolset if it supports it,
	// otherwise fall back to ListTools + find (for non-MCPClient implementations).
	type directCaller interface {
		CallToolDirect(ctx context.Context, runContext map[string]any, tool *agents.BaseTool, params *agents.ToolCall) (*agents.ToolCallResponse, error)
	}

	if dc, ok := t.wrappedMcpServer.(directCaller); ok {
		return dc.CallToolDirect(ctx, t.runContext, t.BaseTool, params)
	}

	// Fallback: ListTools uses schema cache so this is still fast
	mcpTools, err := t.wrappedMcpServer.ListTools(ctx, t.runContext)
	if err != nil {
		return nil, err
	}
	for _, tool := range mcpTools {
		td := tool.GetToolDescriptor()
		if td != nil && td.ToolUnion.OfFunction != nil && params.Name == td.ToolUnion.OfFunction.Name {
			return tool.Execute(ctx, params)
		}
	}
	return nil, err
}

// ToolsetAuthErrorCode marks a listing that failed because the server turned
// our credentials away. It is 401 on purpose — the status the MCP authorization
// spec has a server answer with, and the one the transport actually read.
// Untyped for the same reason ToolCancelledErrorCode is: restate's Code type is
// in an internal package.
const ToolsetAuthErrorCode = 401

// toolsetListError re-states a listing failure in restate's terms, inside the
// step. An *agents.ToolsetError does not survive the step boundary, but an
// error code does, which is what carries the kind out.
//
// Terminal, so restate does not retry the step: the credential is wrong, and
// asking again with the same one gets the same answer.
func toolsetListError(err error) error {
	var te *agents.ToolsetError
	if errors.As(err, &te) && te.Kind == agents.ToolsetErrorAuth {
		return restate.TerminalError(err, ToolsetAuthErrorCode)
	}
	return err
}

// toolsetListErrorFrom is the other half, outside the step: it restores the
// kind that toolsetListError put on the wire, so the agent classifies a listing
// failure here the same way it would running locally.
func toolsetListErrorFrom(err error) error {
	if restate.ErrorCode(err) == ToolsetAuthErrorCode {
		return agents.NewToolsetError(agents.ToolsetErrorAuth, err)
	}
	return err
}
