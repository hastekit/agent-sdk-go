package agents

import (
	"context"
)

// DeferredToolInfo is the projection of a deferred Tool that prompt
// providers actually consume — just the schema's name and description.
// Kept as a plain struct (not the Tool interface) so Dependencies can
// JSON-roundtrip across Temporal activity boundaries; the full Tool
// interface carries closure state (workflow ctx, broker handles, etc.)
// that doesn't deserialize on the worker side.
type DeferredToolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type Dependencies struct {
	RunContext    map[string]any
	Handoffs      []*Handoff
	DeferredTools []DeferredToolInfo

	// Skills contains the enabled catalog for this execution. Legacy providers
	// supply their own hint; dynamic sets share read_skill. Only serializable
	// metadata crosses a durable prompt boundary, never resolver functions.
	Skills    []Skill `json:"skills,omitempty"`
	SkillHint string  `json:"skill_hint,omitempty"`

	// Connectors is how each configured MCP server fared when its tools were
	// listed for this run — see ConnectorStatus. Every one of them is here,
	// connected or not, because a prompt that only ever hears about failures
	// cannot tell the model what it does have.
	//
	// A run with no MCP servers has none, and the resolver that renders these
	// leaves such a prompt untouched.
	Connectors []ConnectorStatus `json:"connectors,omitempty"`
}

type SystemPromptProvider interface {
	GetPrompt(ctx context.Context, data *Dependencies) (string, error)
}
