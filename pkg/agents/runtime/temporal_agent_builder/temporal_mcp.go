package temporal_agent_builder

import (
	"context"
	"errors"
	"fmt"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentconfig"
	"github.com/hastekit/agent-sdk-go/pkg/agents/mcpclient"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/temporal_runtime"
	"go.temporal.io/sdk/workflow"
)

const (
	listMCPToolsActivity   = WorkflowName + "_ListMCPToolsActivity"
	executeMCPToolActivity = WorkflowName + "_ExecuteMCPToolActivity"
)

// MCPListInput is the MCP listing activity's input: the config's connector
// picks, and the run's discovery inputs. RunContext is the run's own, as
// agents.AgentInput carries it.
type MCPListInput struct {
	Connectors map[string]mcpclient.ToolFilter `json:"connectors"`
	Namespace  string                          `json:"namespace"`
	RunContext map[string]any                  `json:"run_context,omitempty"`
	Selection  agents.MCPSelection             `json:"selection,omitempty"`
}

// MCPCallInput is the MCP tool activity's input: the config's connector picks,
// the tool as listed, and the call.
type MCPCallInput struct {
	Connectors map[string]mcpclient.ToolFilter `json:"connectors"`
	Tool       agents.BaseTool                 `json:"tool"`
	Call       *agents.ToolCall                `json:"call"`
	RunContext map[string]any                  `json:"run_context,omitempty"`
}

// mcpClient rebuilds the config's MCP client: the Builder's stores filtered to
// the picks, with the Builder's credentials.
func (a *AgentBuilder) mcpClient(connectors map[string]mcpclient.ToolFilter) (*temporal_runtime.TemporalMCPClient, error) {
	client := a.builder.MCPClient(connectors)
	if client == nil {
		return nil, configError(fmt.Errorf("%w: the worker has no MCP stores for this agent's connectors", agentconfig.ErrInvalid))
	}
	return temporal_runtime.NewTemporalMCPClient(client, a.broker, agents.ToolCallMiddlewaresOf(a.builder.Middlewares())...), nil
}

// ListMCPTools lists the picked connectors' tools, resolving configuration and
// credentials only here, on the worker.
func (a *AgentBuilder) ListMCPTools(ctx context.Context, in *MCPListInput) (*agents.MCPListing, error) {
	client, err := a.mcpClient(in.Connectors)
	if err != nil {
		return nil, err
	}
	return client.ListTools(ctx, &agents.AgentInput{Namespace: in.Namespace, RunContext: in.RunContext, MCP: in.Selection})
}

// ExecuteMCPTool calls one listed tool through the same filtered client, so a
// tool left out of the picks cannot be called.
func (a *AgentBuilder) ExecuteMCPTool(ctx context.Context, in *MCPCallInput) (*agents.ToolCallResponse, error) {
	client, err := a.mcpClient(in.Connectors)
	if err != nil {
		return nil, err
	}
	return client.ExecuteTool(ctx, &in.Tool, in.Call, in.RunContext)
}

// mcpClientProxy is the workflow-side MCP client: listing runs in a
// ListMCPTools activity carrying the config's picks, and each tool it lists
// runs in an ExecuteMCPTool activity carrying them again.
type mcpClientProxy struct {
	ctx        workflow.Context
	connectors map[string]mcpclient.ToolFilter
}

func newMCPClientProxy(ctx workflow.Context, connectors map[string]mcpclient.ToolFilter) agents.MCPClient {
	return &mcpClientProxy{ctx: ctx, connectors: connectors}
}

func (c *mcpClientProxy) ListTools(_ context.Context, namespace string, runContext map[string]any, selection ...agents.MCPSelection) ([]agents.ConnectorStatus, []agents.Tool, error) {
	if len(selection) > 1 {
		return nil, nil, errors.New("MCP listing accepts at most one selection")
	}
	input := &MCPListInput{Connectors: c.connectors, Namespace: namespace, RunContext: runContext}
	if len(selection) == 1 {
		input.Selection = selection[0]
	}
	var listing agents.MCPListing
	if err := workflow.ExecuteActivity(c.ctx, listMCPToolsActivity, input).Get(c.ctx, &listing); err != nil {
		return nil, nil, err
	}
	tools := make([]agents.Tool, 0, len(listing.Tools))
	for _, tool := range listing.Tools {
		tools = append(tools, &mcpToolProxy{ctx: c.ctx, connectors: c.connectors, runContext: runContext, BaseTool: tool})
	}
	return listing.Connectors, tools, nil
}

// CallTool is not used by the workflow: listed tools run their own activity.
func (c *mcpClientProxy) CallTool(context.Context, *agents.BaseTool, *agents.ToolCall) (*agents.ToolCallResponse, error) {
	return nil, errors.New("direct MCP calls are not supported by the workflow proxy; execute a tool returned by ListTools")
}

// mcpToolProxy is one listed MCP tool in the workflow.
type mcpToolProxy struct {
	ctx        workflow.Context
	connectors map[string]mcpclient.ToolFilter
	runContext map[string]any
	agents.BaseTool
}

func (t *mcpToolProxy) Execute(_ context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	var output *agents.ToolCallResponse
	input := &MCPCallInput{Connectors: t.connectors, Tool: t.BaseTool, Call: params, RunContext: t.runContext}
	if err := workflow.ExecuteActivity(t.ctx, executeMCPToolActivity, input).Get(t.ctx, &output); err != nil {
		return nil, err
	}
	return output, nil
}

func (t *mcpToolProxy) GetToolDescriptor() *agents.BaseTool { return &t.BaseTool }
