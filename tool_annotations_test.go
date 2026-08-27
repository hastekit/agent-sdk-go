package sdk

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
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

	annotations := annotationsOf(t, tool)
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

	annotations := annotationsOf(t, tool)
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

	annotations := annotationsOf(t, tool)
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
	assert.True(t, got.Annotations.IsReadOnly())
}

// annotationsOf reads a tool's annotations the one way there is: through the
// BaseTool it reports. Nil is a usable answer — every Is* helper is nil-safe.
func annotationsOf(t *testing.T, tool agents.Tool) *agents.ToolAnnotations {
	t.Helper()
	base, err := tool.GetBaseTool()
	require.NoError(t, err)
	return base.Annotations
}

// Meta rides along on the BaseTool, which is what a tool call hook is shown —
// so a policy can key off where a tool came from without recognising it by name.
func TestFunctionToolMeta(t *testing.T) {
	tool := NewTool(lookup,
		WithName("search"),
		WithMeta(map[string]any{"team": "search-infra", "tier": "internal"}),
	)

	base, err := tool.GetBaseTool()
	require.NoError(t, err)
	assert.Equal(t, "search-infra", base.Meta["team"])
	assert.Equal(t, "internal", base.Meta["tier"])
}

// Several options can each contribute a key, and a later one wins where both
// set the same key — the same way the annotation hints compose.
func TestFunctionToolMetaComposes(t *testing.T) {
	tool := NewTool(lookup,
		WithMeta(map[string]any{"team": "search-infra", "tier": "internal"}),
		WithMeta(map[string]any{"tier": "public"}),
	)

	base, err := tool.GetBaseTool()
	require.NoError(t, err)
	assert.Equal(t, "search-infra", base.Meta["team"], "an earlier key survives")
	assert.Equal(t, "public", base.Meta["tier"], "a later one wins where they collide")
}

// The option works on a copy, so one map handed to several tools does not pick
// up one tool's keys on another.
func TestFunctionToolMetaDoesNotAliasTheCallersMap(t *testing.T) {
	shared := map[string]any{"team": "search-infra"}

	first := NewTool(lookup, WithMeta(shared))
	second := NewTool(lookup, WithMeta(shared), WithMeta(map[string]any{"tier": "public"}))

	firstBase, err := first.GetBaseTool()
	require.NoError(t, err)
	secondBase, err := second.GetBaseTool()
	require.NoError(t, err)

	assert.NotContains(t, firstBase.Meta, "tier", "the second tool's key did not leak into the first")
	assert.Equal(t, "public", secondBase.Meta["tier"])
	assert.Equal(t, map[string]any{"team": "search-infra"}, shared, "the caller's own map is untouched")
}

// A tool with no meta reports none rather than an empty map to rummage through.
func TestFunctionToolWithoutMeta(t *testing.T) {
	base, err := NewTool(lookup).GetBaseTool()
	require.NoError(t, err)
	assert.Nil(t, base.Meta)
}
