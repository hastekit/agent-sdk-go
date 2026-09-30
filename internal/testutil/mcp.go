// Package testutil supplies in-process fixtures for SDK tests.
package testutil

import (
	"context"
	"errors"
	"sort"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
)

// MCPClient adapts test toolsets to the agent's catalog boundary.
type MCPClient struct{ servers []agents.MCPToolset }

func NewMCPClient(servers ...agents.MCPToolset) *MCPClient {
	return &MCPClient{servers: servers}
}

func (c *MCPClient) ListTools(ctx context.Context, namespace string, runContext map[string]any, _ ...agents.MCPSelection) ([]agents.ConnectorStatus, []agents.Tool, error) {
	// Keep the fixture ordering deterministic like a real catalog.
	servers := append([]agents.MCPToolset(nil), c.servers...)
	sort.Slice(servers, func(i, j int) bool { return servers[i].GetName() < servers[j].GetName() })
	var statuses []agents.ConnectorStatus
	var tools []agents.Tool
	for _, server := range servers {
		listed, err := server.ListTools(ctx, namespace, runContext)
		if err != nil {
			statuses = append(statuses, agents.FailedConnectorStatus(server.GetName(), err))
			continue
		}
		statuses = append(statuses, agents.ConnectedConnectorStatus(server.GetName(), len(listed)))
		tools = append(tools, listed...)
	}
	return statuses, tools, nil
}

func (c *MCPClient) CallTool(ctx context.Context, descriptor *agents.BaseTool, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
	// Route test calls to their fixture tool without creating a network connection.
	_, tools, err := c.ListTools(ctx, call.Namespace, call.RunContext)
	if err != nil {
		return nil, err
	}
	for _, tool := range tools {
		if tool.GetToolDescriptor().ToolUnion.OfFunction.Name == descriptor.ToolUnion.OfFunction.Name {
			return tool.Execute(ctx, call)
		}
	}
	return nil, errors.New("unknown fixture tool")
}
