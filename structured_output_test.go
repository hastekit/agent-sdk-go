package sdk

import (
	"context"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/stretchr/testify/require"
)

type schemaNamed struct {
	Query string        `json:"query"`
	Inner schemaInner   `json:"inner"`
	Items []schemaInner `json:"items"`
}

type schemaInner struct {
	N int `json:"n"`
}

// Every Go type reflects to a schema. Only a named struct used to: anything
// else — an anonymous struct above all, the natural input of a tool that takes
// no arguments — panicked inside the reflector, and since a tool's schema is
// built whenever the tool describes itself, that took a durable worker down at
// startup.
func TestNewOutputSchemaReflectsEveryType(t *testing.T) {
	inner := map[string]any{
		"type": "object", "additionalProperties": false, "required": []any{"n"},
		"properties": map[string]any{"n": map[string]any{"type": "integer"}},
	}
	named := map[string]any{
		"type": "object", "additionalProperties": false, "required": []any{"query", "inner", "items"},
		"properties": map[string]any{
			"query": map[string]any{"type": "string"},
			"inner": inner,
			"items": map[string]any{"type": "array", "items": inner},
		},
	}

	for _, tc := range []struct {
		name string
		v    any
		want map[string]any
	}{
		{"named struct", schemaNamed{}, named},
		{"pointer to a named struct", &schemaNamed{}, named},
		{"empty anonymous struct", struct{}{}, map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}}},
		{"anonymous struct", struct {
			Inner schemaInner `json:"inner"`
		}{}, map[string]any{
			"type": "object", "additionalProperties": false, "required": []any{"inner"},
			"properties": map[string]any{"inner": inner},
		}},
		{"pointer to an anonymous struct", &struct {
			On bool `json:"on"`
		}{}, map[string]any{
			"type": "object", "additionalProperties": false, "required": []any{"on"},
			"properties": map[string]any{"on": map[string]any{"type": "boolean"}},
		}},
		{"map", map[string]any{}, map[string]any{"type": "object"}},
		{"slice", []schemaInner{}, map[string]any{"type": "array", "items": inner}},
		{"string", "", map[string]any{"type": "string"}},
		{"nil, as an any input is", nil, map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, NewOutputSchema(tc.v))
		})
	}
}

// A tool that takes no arguments can say so with an anonymous struct, and a
// background tool alike.
func TestToolsWithAnonymousInputDescribeThemselves(t *testing.T) {
	empty := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}}

	tool := NewTool(func(context.Context, struct{}) (string, error) { return "done", nil }, WithName("ping"))
	require.Equal(t, empty, tool.GetToolDescriptor().ToolUnion.OfFunction.Parameters)

	background := NewBackgroundTool(func(context.Context, struct{}, agents.ProgressReporter) (string, error) { return "done", nil }, WithName("reindex"))
	require.Equal(t, empty, background.GetToolDescriptor().ToolUnion.OfFunction.Parameters)
}
