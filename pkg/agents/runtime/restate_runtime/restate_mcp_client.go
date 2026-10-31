package restate_runtime

import (
	"context"
	"errors"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	restate "github.com/restatedev/sdk-go"
)

// RestateMCPClient journals discovery once for the entire dynamic server catalog.
type RestateMCPClient struct {
	ctx         restate.WorkflowContext
	client      agents.MCPClient
	middlewares []agents.ToolCallMiddleware
}

func NewRestateMCPClient(ctx restate.WorkflowContext, client agents.MCPClient, broker agents.StreamBroker, middlewares ...agents.ToolCallMiddleware) *RestateMCPClient {
	return &RestateMCPClient{ctx: ctx, client: client, middlewares: append([]agents.ToolCallMiddleware{agents.StopMiddleware{Watcher: agents.StopWatcherFrom(broker)}, agents.TimestampMiddleware{}}, middlewares...)}
}

func (c *RestateMCPClient) ListTools(_ context.Context, namespace string, runContext map[string]any, selection ...agents.MCPSelection) ([]agents.ConnectorStatus, []agents.Tool, error) {
	// Configurations and credentials stay inside the step; only descriptors and statuses are journaled.
	if len(selection) > 1 {
		return nil, nil, errors.New("MCP listing accepts at most one selection")
	}
	result, err := restate.Run(c.ctx, func(ctx restate.RunContext) (*agents.MCPListing, error) {
		return c.listTools(ctx, namespace, runContext, selection...)
	}, restate.WithName("MCPListTools"))
	if err != nil {
		return nil, nil, err
	}
	tools := make([]agents.Tool, 0, len(result.Tools))
	for _, tool := range result.Tools {
		tools = append(tools, &restateMCPClientTool{BaseTool: &tool, client: c})
	}
	return result.Connectors, tools, nil
}

// listTools is the discovery step body, keeping transport objects outside the journal.
func (c *RestateMCPClient) listTools(ctx context.Context, namespace string, runContext map[string]any, selection ...agents.MCPSelection) (*agents.MCPListing, error) {
	// Resolve the current catalog and preserve connector failures as status data.
	statuses, tools, err := c.client.ListTools(ctx, namespace, runContext, selection...)
	if err != nil {
		return nil, err
	}

	// Serialize complete descriptors so routing, annotations, and metadata survive replay.
	result := &agents.MCPListing{Connectors: statuses}
	for _, tool := range tools {
		if descriptor := tool.GetToolDescriptor(); descriptor != nil {
			result.Tools = append(result.Tools, *descriptor)
		}
	}
	return result, nil
}

// CallTool is used by returned tool wrappers to execute inside a durable step.
func (c *RestateMCPClient) CallTool(_ context.Context, tool *agents.BaseTool, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
	return restate.Run(c.ctx, func(ctx restate.RunContext) (*agents.ToolCallResponse, error) {
		return c.executeTool(ctx, tool, call)
	}, restate.WithName("MCPToolCall"))
}

// executeTool runs tracing, middleware, and the MCP request inside the step body.
func (c *RestateMCPClient) executeTool(ctx context.Context, tool *agents.BaseTool, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
	result, err := agents.ExecuteWithTrace(ctx, nil, call, func(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
		return agents.ExecuteToolCallWithMiddleware(ctx, c.middlewares, agents.SerializeTool(tool, call), call, func(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
			return c.client.CallTool(ctx, tool, call)
		})
	})
	return result, cancellationError(err)
}

type restateMCPClientTool struct {
	*agents.BaseTool
	client *RestateMCPClient
}

func (t *restateMCPClientTool) Execute(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
	return t.client.CallTool(ctx, t.BaseTool, call)
}

var _ agents.MCPClient = (*RestateMCPClient)(nil)
