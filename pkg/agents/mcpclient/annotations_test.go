package mcpclient

import (
	"testing"

	"github.com/hastekit/hastekit-sdk-go/pkg/agents"
	"github.com/hastekit/hastekit-sdk-go/pkg/utils"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A server that annotates a tool as read-only has that carried through to the
// agent-side tool, where a permission policy can read it.
func TestToolAnnotationsReadThrough(t *testing.T) {
	tool := NewMcpTool(&mcp.Tool{
		Name: "search",
		Annotations: &mcp.ToolAnnotations{
			Title:        "Search",
			ReadOnlyHint: true,
		},
	}, nil, nil, false, false)

	annotations := agents.AnnotationsOf(tool)
	require.NotNil(t, annotations)
	assert.Equal(t, "Search", annotations.Title)
	assert.True(t, annotations.IsReadOnly())
	assert.False(t, annotations.IsDestructive(), "a read-only tool is never destructive")
	assert.True(t, annotations.IsIdempotent(), "a read-only tool is idempotent by definition")
}

// destructiveHint arrives as a pointer, so a server saying "not destructive"
// is distinguishable from a server saying nothing — and only the first one
// gets the benefit of the doubt.
func TestToolAnnotationsDestructive(t *testing.T) {
	additive := NewMcpTool(&mcp.Tool{
		Name:        "append_row",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: utils.Ptr(false)},
	}, nil, nil, false, false)
	assert.False(t, agents.AnnotationsOf(additive).IsDestructive())

	silent := NewMcpTool(&mcp.Tool{
		Name:        "do_something",
		Annotations: &mcp.ToolAnnotations{},
	}, nil, nil, false, false)
	assert.True(t, agents.AnnotationsOf(silent).IsDestructive(), "an unstated hint is destructive")
}

// A tool with no annotations block at all leaves the SDK's annotations nil,
// and the nil still answers with the conservative defaults.
func TestToolAnnotationsAbsent(t *testing.T) {
	tool := NewMcpTool(&mcp.Tool{Name: "mystery"}, nil, nil, false, false)

	annotations := agents.AnnotationsOf(tool)
	assert.Nil(t, annotations)
	assert.False(t, annotations.IsReadOnly())
	assert.True(t, annotations.IsDestructive())
	assert.True(t, annotations.IsOpenWorld())
}

// The lazy tools ListTools hands back carry annotations too — that's the path
// every cached/durable run takes.
func TestLazyToolAnnotations(t *testing.T) {
	tool := NewLazyMcpTool(&mcp.Tool{
		Name:        "read_file",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: utils.Ptr(false)},
	}, "https://example.test/mcp", "streamable-http", nil, nil, false, false, false)

	annotations := agents.AnnotationsOf(tool)
	require.NotNil(t, annotations)
	assert.True(t, annotations.IsReadOnly())
	assert.False(t, annotations.IsOpenWorld())
}

// The tools ListTools hands back carry each server tool's annotations, which
// is what a later permission layer will read them from.
func TestBuildLazyToolsKeepsAnnotations(t *testing.T) {
	srv := &MCPClient{}

	tools := srv.buildLazyTools([]*mcp.Tool{
		{Name: "search", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		{Name: "delete_all", Annotations: &mcp.ToolAnnotations{DestructiveHint: utils.Ptr(true)}},
		{Name: "unannotated"},
	}, nil, nil)

	require.Len(t, tools, 3)
	assert.True(t, agents.AnnotationsOf(tools[0]).IsReadOnly())
	assert.True(t, agents.AnnotationsOf(tools[1]).IsDestructive())
	assert.Nil(t, agents.AnnotationsOf(tools[2]))
}
