package temporal_runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

type TemporalMCPServer struct {
	middlewares      []agents.ToolCallMiddleware
	wrappedMcpServer agents.MCPToolset
	broker           agents.StreamBroker
}

func NewTemporalMCPServer(wrappedMcpServer agents.MCPToolset, broker agents.StreamBroker, middlewares ...agents.ToolCallMiddleware) *TemporalMCPServer {
	return &TemporalMCPServer{
		wrappedMcpServer: wrappedMcpServer,
		broker:           broker,
		middlewares:      append([]agents.ToolCallMiddleware{agents.StopMiddleware{Watcher: agents.StopWatcherFrom(broker)}}, middlewares...),
	}
}

func (t *TemporalMCPServer) ListTools(ctx context.Context, runContext map[string]any) ([]agents.BaseTool, error) {
	mcpTools, err := t.wrappedMcpServer.ListTools(ctx, runContext)
	if err != nil {
		return nil, toolsetListError(err)
	}

	// GetToolDescriptor rather than a field-by-field copy: this is the only crossing a
	// tool makes into the workflow, and anything left out here is gone for good
	// on the far side — the tool's own name and its meta included, which is
	// exactly what a middleware over there is looking at.
	var tools []agents.BaseTool
	for _, tool := range mcpTools {
		if encoded := tool.GetToolDescriptor(); encoded != nil {
			tools = append(tools, *encoded)
		}
	}

	return tools, nil
}

// ExecuteTool is the _ExecuteMCPToolActivity implementation. It runs inside a
// Temporal activity, so middleware and tracing run on each activity attempt,
// never during workflow replay. callTool does the real work.
func (t *TemporalMCPServer) ExecuteTool(ctx context.Context, tool *agents.BaseTool, params *agents.ToolCall, runContext map[string]any) (*agents.ToolCallResponse, error) {
	injectProgressReporter(ctx, t.broker, params)

	resp, err := agents.ExecuteWithTrace(ctx, nil, params, func(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
		return agents.ExecuteToolCallWithMiddleware(ctx, t.middlewares, tool, call, func(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
			return t.callTool(ctx, tool, call, runContext)
		})
	})

	return resp, cancellationError(err)
}

func (t *TemporalMCPServer) callTool(ctx context.Context, tool *agents.BaseTool, params *agents.ToolCall, runContext map[string]any) (*agents.ToolCallResponse, error) {
	// Use CallToolDirect if the wrapped MCPToolset supports it (e.g. MCPClient),
	// which calls the tool directly via the connection pool without re-listing.
	// The tool crossed the boundary with the call, so nothing has to be resolved
	// by name on this side.
	type directCaller interface {
		CallToolDirect(ctx context.Context, runContext map[string]any, tool *agents.BaseTool, params *agents.ToolCall) (*agents.ToolCallResponse, error)
	}

	if dc, ok := t.wrappedMcpServer.(directCaller); ok {
		return dc.CallToolDirect(ctx, runContext, tool, params)
	}

	// Fallback: ListTools uses schema cache so this is still efficient
	mcpTools, err := t.wrappedMcpServer.ListTools(ctx, runContext)
	if err != nil {
		return nil, err
	}

	for _, tool := range mcpTools {
		td := tool.GetToolDescriptor()
		if td != nil && td.ToolUnion.OfFunction != nil && params.Name == td.ToolUnion.OfFunction.Name {
			return tool.Execute(ctx, params)
		}
	}

	return nil, fmt.Errorf("no tool found with name %s", params.Name)
}

type TemporalMCPProxy struct {
	name        string
	workflowCtx workflow.Context
	prefix      string
}

func NewTemporalMCPProxy(workflowCtx workflow.Context, prefix string) *TemporalMCPProxy {
	return &TemporalMCPProxy{
		workflowCtx: workflowCtx,
		name:        prefix,
		prefix:      prefix,
	}
}

func (t *TemporalMCPProxy) GetName() string {
	return t.name
}

func (t *TemporalMCPProxy) ListTools(ctx context.Context, runContext map[string]any) ([]agents.Tool, error) {
	var toolDefs []agents.BaseTool
	err := workflow.ExecuteActivity(t.workflowCtx, t.prefix+"_ListMCPToolsActivity", runContext).Get(t.workflowCtx, &toolDefs)
	if err != nil {
		return nil, toolsetListErrorFrom(err)
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
	// The tool goes with the call: the activity side would otherwise have to
	// find it by name, and the name it has is the model-facing one.
	err := workflow.ExecuteActivity(t.workflowCtx, t.prefix+"_ExecuteMCPToolActivity", t.BaseTool, params, t.runContext).Get(t.workflowCtx, &output)
	if err != nil {
		return nil, err
	}

	return output, nil
}

// ToolsetAuthErrorType marks a listing that failed because the server turned
// our credentials away. Non-retryable: the credential is wrong, and asking a
// second time with the same one gets the same answer — the agent is told the
// server is unavailable instead, and the run goes on without its tools.
const ToolsetAuthErrorType = "ToolsetAuthError"

// toolsetListError re-states a listing failure in Temporal's terms, on the
// activity side. An *agents.ToolsetError does not survive the crossing back
// into the workflow — the error is flattened to its message — but an
// ApplicationError's type does, which is what carries the kind over.
func toolsetListError(err error) error {
	var te *agents.ToolsetError
	if errors.As(err, &te) && te.Kind == agents.ToolsetErrorAuth {
		return temporal.NewNonRetryableApplicationError(err.Error(), ToolsetAuthErrorType, nil)
	}
	return err
}

// toolsetListErrorFrom is the other half, on the workflow side: it restores the
// kind that toolsetListError put on the wire, so the agent classifies a
// listing failure here the same way it would running locally.
func toolsetListErrorFrom(err error) error {
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) && appErr.Type() == ToolsetAuthErrorType {
		return agents.NewToolsetError(agents.ToolsetErrorAuth, err)
	}
	return err
}
