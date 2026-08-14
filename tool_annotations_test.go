package sdk

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hastekit/hastekit-sdk-go/pkg/agents"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type lookupIn struct {
	Query string `json:"query"`
}

func lookup(_ context.Context, in lookupIn) (string, error) { return in.Query, nil }

// A locally defined tool annotates itself the same way an MCP server does, so
// one policy reads both.
func TestFunctionToolAnnotations(t *testing.T) {
	tool := NewTool(lookup, WithName("lookup"), WithReadOnly(true), WithTitle("Lookup"))

	annotations := agents.AnnotationsOf(tool)
	require.NotNil(t, annotations)
	assert.Equal(t, "Lookup", annotations.Title)
	assert.True(t, annotations.IsReadOnly())
	assert.False(t, annotations.IsDestructive())
}

// The single-hint options amend the set rather than replacing it, so several
// of them can be passed to the same tool.
func TestFunctionToolAnnotationsCompose(t *testing.T) {
	tool := NewTool(lookup,
		WithName("append_row"),
		WithDestructive(false),
		WithIdempotent(true),
		WithOpenWorld(false),
	)

	annotations := agents.AnnotationsOf(tool)
	require.NotNil(t, annotations)
	assert.False(t, annotations.IsReadOnly(), "not claimed, so not assumed")
	assert.False(t, annotations.IsDestructive())
	assert.True(t, annotations.IsIdempotent())
	assert.False(t, annotations.IsOpenWorld())
}

// A tool that says nothing gets nil annotations, and the nil still answers
// with the conservative MCP defaults.
func TestFunctionToolWithoutAnnotations(t *testing.T) {
	tool := NewTool(lookup, WithName("mystery"))

	annotations := agents.AnnotationsOf(tool)
	assert.Nil(t, annotations)
	assert.False(t, annotations.IsReadOnly())
	assert.True(t, annotations.IsDestructive())
}

// Annotations ride along in BaseTool, which is how they survive a durable
// runtime's serialization boundary.
func TestAnnotationsSurviveBaseToolRoundTrip(t *testing.T) {
	readOnly := true
	sent := agents.BaseTool{Annotations: &agents.ToolAnnotations{ReadOnlyHint: &readOnly, Title: "Search"}}

	encoded, err := json.Marshal(sent)
	require.NoError(t, err)

	var got agents.BaseTool
	require.NoError(t, json.Unmarshal(encoded, &got))

	require.NotNil(t, got.Annotations)
	assert.Equal(t, "Search", got.Annotations.Title)
	assert.True(t, got.GetAnnotations().IsReadOnly())
}
