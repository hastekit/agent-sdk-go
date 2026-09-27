package restate_runtime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/require"
)

// namespaceMCPClient captures the inputs crossing the catalog's durable step bodies.
type namespaceMCPClient struct {
	namespace  string
	runContext map[string]any
	selection  []agents.MCPSelection
	server     string
}

func (c *namespaceMCPClient) ListTools(_ context.Context, namespace string, rc map[string]any, selection ...agents.MCPSelection) ([]agents.ConnectorStatus, []agents.Tool, error) {
	c.namespace, c.runContext, c.selection = namespace, rc, selection
	return []agents.ConnectorStatus{
			{Name: "mail", Kind: agents.ToolsetErrorAuth},
			{Name: "down", Kind: agents.ToolsetErrorUnavailable, Detail: "connection refused"},
		}, []agents.Tool{&transformMediaTool{BaseTool: &agents.BaseTool{
			Name: "read", MCPServerName: "docs", RequiresApproval: true,
			ToolUnion:   responses.ToolUnion{OfFunction: &responses.FunctionTool{Name: "docs__read"}},
			Annotations: &agents.ToolAnnotations{ReadOnlyHint: utils.Ptr(true)},
			Meta:        map[string]any{"source": "catalog"},
		}}}, nil
}

func (c *namespaceMCPClient) CallTool(_ context.Context, tool *agents.BaseTool, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
	c.namespace, c.runContext, c.server = call.Namespace, call.RunContext, tool.MCPServerName
	return agents.ToolCallResult(call, "read complete"), nil
}

func TestRestateMCPJournalPreservesDiscoveryAndRouting(t *testing.T) {
	// The listing body receives authoritative identity separately from free-form context.
	client := &namespaceMCPClient{}
	wrapper := NewRestateMCPClient(nil, client, nil)
	rc := map[string]any{"namespace": "spoofed"}
	selection := agents.MCPSelection{Disable: []string{"disabled"}}
	listing, err := wrapper.listTools(t.Context(), "authenticated-user", rc, selection)
	require.NoError(t, err)
	require.Equal(t, "authenticated-user", client.namespace)
	require.Equal(t, rc, client.runContext)
	require.Equal(t, []agents.MCPSelection{selection}, client.selection)

	// Journal serialization preserves failure classifications and complete tool descriptors.
	wire, err := json.Marshal(listing)
	require.NoError(t, err)
	var restored agents.MCPListing
	require.NoError(t, json.Unmarshal(wire, &restored))
	require.Equal(t, agents.ToolsetErrorAuth, restored.Connectors[0].Kind)
	require.Equal(t, agents.ToolsetErrorUnavailable, restored.Connectors[1].Kind)
	require.Equal(t, "connection refused", restored.Connectors[1].Detail)
	require.Len(t, restored.Tools, 1)
	require.Equal(t, "docs", restored.Tools[0].MCPServerName)
	require.True(t, restored.Tools[0].Annotations.IsReadOnly())
	require.True(t, restored.Tools[0].RequiresApproval)
	require.Equal(t, "catalog", restored.Tools[0].Meta["source"])

	// Execution delegates directly to the catalog client using the serialized routing field.
	result, err := wrapper.executeTool(t.Context(), &restored.Tools[0], &agents.ToolCall{
		Namespace: "authenticated-user", RunContext: rc,
		FunctionCallMessage: &responses.FunctionCallMessage{Name: "docs__read", CallID: "call", Arguments: "{}"},
	})
	require.NoError(t, err)
	require.Equal(t, "read complete", *result.Output.OfString)
	require.Equal(t, "authenticated-user", client.namespace)
	require.Equal(t, "spoofed", client.runContext["namespace"])
	require.Equal(t, "docs", client.server)
}
