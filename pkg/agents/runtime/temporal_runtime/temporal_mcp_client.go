package temporal_runtime

import (
	"context"
	"errors"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"go.temporal.io/sdk/workflow"
)

// TemporalMCPClient uses fixed activities for an entire catalog, including dynamically stored servers.
type TemporalMCPClient struct {
	client      agents.MCPClient
	broker      agents.StreamBroker
	middlewares []agents.ToolCallMiddleware
}

func NewTemporalMCPClient(client agents.MCPClient, broker agents.StreamBroker, middlewares ...agents.ToolCallMiddleware) *TemporalMCPClient {
	return &TemporalMCPClient{client: client, broker: broker, middlewares: append([]agents.ToolCallMiddleware{agents.StopMiddleware{Watcher: agents.StopWatcherFrom(broker)}, agents.TimestampMiddleware{}}, middlewares...)}
}

// ListTools resolves configuration and credentials only inside the activity.
func (c *TemporalMCPClient) ListTools(ctx context.Context, in *agents.AgentInput) (*agents.MCPListing, error) {
	statuses, tools, err := c.client.ListTools(ctx, in.Namespace, in.RunContext, in.MCP)
	if err != nil {
		return nil, err
	}
	result := &agents.MCPListing{Connectors: statuses}
	for _, tool := range tools {
		if descriptor := tool.GetToolDescriptor(); descriptor != nil {
			result.Tools = append(result.Tools, *descriptor)
		}
	}
	return result, nil
}

func (c *TemporalMCPClient) ExecuteTool(ctx context.Context, tool *agents.BaseTool, call *agents.ToolCall, runContext map[string]any) (*agents.ToolCallResponse, error) {
	// Restore runtime-only state before invoking middleware and the shared catalog client.
	if call.RunContext == nil {
		call.RunContext = runContext
	}
	injectProgressReporter(ctx, c.broker, call)
	result, err := agents.ExecuteWithTrace(ctx, nil, call, func(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
		return agents.ExecuteToolCallWithMiddleware(ctx, c.middlewares, agents.SerializeTool(tool, call), call, func(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
			return c.client.CallTool(ctx, tool, call)
		})
	})
	return result, cancellationError(err)
}

type temporalMCPClientProxy struct {
	ctx    workflow.Context
	prefix string
}

func (c *temporalMCPClientProxy) ListTools(_ context.Context, namespace string, runContext map[string]any, selection ...agents.MCPSelection) ([]agents.ConnectorStatus, []agents.Tool, error) {
	// Only discovery inputs cross the activity boundary; model messages and secrets are unnecessary.
	input := &agents.AgentInput{Namespace: namespace, RunContext: runContext}
	if len(selection) > 1 {
		return nil, nil, errors.New("MCP listing accepts at most one selection")
	}
	if len(selection) == 1 {
		input.MCP = selection[0]
	}
	var result agents.MCPListing
	if err := workflow.ExecuteActivity(c.ctx, c.prefix+"_ListMCPToolsActivity", input).Get(c.ctx, &result); err != nil {
		return nil, nil, err
	}
	tools := make([]agents.Tool, 0, len(result.Tools))
	for _, tool := range result.Tools {
		tools = append(tools, NewTemporalMCPToolProxy(c.ctx, c.prefix, runContext, tool))
	}
	return result.Connectors, tools, nil
}

// CallTool is not used by the workflow: discovered tools own activity execution.
func (c *temporalMCPClientProxy) CallTool(context.Context, *agents.BaseTool, *agents.ToolCall) (*agents.ToolCallResponse, error) {
	return nil, errors.New("direct MCP calls are not supported by the Temporal workflow proxy; execute a tool returned by ListTools")
}

var _ agents.MCPClient = (*temporalMCPClientProxy)(nil)
