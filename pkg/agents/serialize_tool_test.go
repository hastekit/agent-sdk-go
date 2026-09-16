package agents_test

import (
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

// A function tool leaves Name empty because its one name is the one in
// ToolUnion. A middleware is shown it filled in anyway, on a copy: the
// descriptor may be the tool's own embedded BaseTool, which is not ours to
// write to.
func TestSerializeTool_FillsAFunctionToolsName(t *testing.T) {
	descriptor := &agents.BaseTool{ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{Name: "search"}}}

	shown := agents.SerializeTool(descriptor, &agents.ToolCall{FunctionCallMessage: &responses.FunctionCallMessage{Name: "search"}})

	require.Equal(t, "search", shown.Name)
	require.Equal(t, "search", shown.ToolUnion.OfFunction.Name)
	require.Empty(t, descriptor.Name, "the tool's own descriptor is left alone")
}

// A tool with a name of its own — an MCP server's — is shown as it is.
func TestSerializeTool_KeepsANamedDescriptor(t *testing.T) {
	descriptor := &agents.BaseTool{Name: "search", ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{Name: "xyz__search"}}}

	require.Same(t, descriptor, agents.SerializeTool(descriptor, nil))
}

// A tool that cannot describe itself still gets its call checked, so what a
// middleware is handed is never nil, and carries a union the durable runtimes
// can encode.
func TestSerializeTool_NamesANamelessToolFromTheCall(t *testing.T) {
	shown := agents.SerializeTool(nil, &agents.ToolCall{FunctionCallMessage: &responses.FunctionCallMessage{Name: "search"}})

	require.NotNil(t, shown)
	require.Equal(t, "search", shown.Name)
	require.NotNil(t, shown.ToolUnion.OfFunction)
	require.Equal(t, "search", shown.ToolUnion.OfFunction.Name)

	require.NotNil(t, agents.SerializeTool(nil, nil), "never nil, even with nothing to go on")
}
