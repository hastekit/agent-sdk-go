package temporal_runtime_test

import (
	"context"
	"testing"

	"github.com/hastekit/hastekit-sdk-go/pkg/agents"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/runtime/temporal_runtime"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/hastekit-sdk-go/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
)

// annotatedToolset stands in for an MCP server whose tools describe what they
// do.
type annotatedToolset struct{ tools []agents.Tool }

func (s *annotatedToolset) GetName() string { return "annotated" }

func (s *annotatedToolset) ListTools(ctx context.Context, runContext map[string]any) ([]agents.Tool, error) {
	return s.tools, nil
}

// staticTool is a tool that only has to describe itself — these tests are
// about what survives the boundary, not what runs.
type staticTool struct{ *agents.BaseTool }

func (t *staticTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	return &agents.ToolCallResponse{}, nil
}

func annotatedTool(name string, annotations *agents.ToolAnnotations, requiresApproval bool) agents.Tool {
	return &staticTool{BaseTool: &agents.BaseTool{
		RequiresApproval: requiresApproval,
		Annotations:      annotations,
		ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{
			Name:        name,
			Description: utils.Ptr("test tool"),
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		}},
	}}
}

// The workflow side sees MCP tools only as the BaseTools the list activity
// returned, so anything the loop needs to gate a call has to survive the data
// converter. Annotations are what a permission hook reads to decide.
func TestListMCPToolsActivity_CarriesAnnotations(t *testing.T) {
	toolset := &annotatedToolset{tools: []agents.Tool{
		annotatedTool("search", &agents.ToolAnnotations{ReadOnlyHint: utils.Ptr(true)}, false),
		annotatedTool("wipe_disk", &agents.ToolAnnotations{DestructiveHint: utils.Ptr(true)}, false),
		annotatedTool("plain", nil, true),
	}}
	server := temporal_runtime.NewTemporalMCPServer(toolset, nil)

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(server.ListTools)

	val, err := env.ExecuteActivity(server.ListTools, map[string]any{})
	require.NoError(t, err)

	var got []agents.BaseTool
	require.NoError(t, val.Get(&got))
	require.Len(t, got, 3)

	// Read-only survives, so the loop lets it run unattended.
	require.NotNil(t, got[0].Annotations)
	assert.True(t, got[0].GetAnnotations().IsReadOnly())

	// Destructive survives, so the loop still knows to ask.
	assert.True(t, got[1].GetAnnotations().IsDeclaredDestructive())

	// No annotations stays no annotations rather than becoming a claim.
	assert.Nil(t, got[2].Annotations)
	assert.True(t, got[2].NeedApproval(), "the approval flag rides along as before")
}

// The workflow-side proxy wraps a tool rather than embedding its BaseTool, so
// it has to forward the annotations itself or the loop sees an unannotated
// tool where the author declared one.
func TestTemporalToolProxy_ForwardsAnnotations(t *testing.T) {
	tool := annotatedTool("wipe_disk", &agents.ToolAnnotations{DestructiveHint: utils.Ptr(true)}, false)

	proxy := temporal_runtime.NewTemporalToolProxy(nil, "prefix", tool)

	assert.True(t, agents.AnnotationsOf(proxy).IsDeclaredDestructive())
}
