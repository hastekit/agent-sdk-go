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

	// Skills the agent was given, and the name of the tool that reads them —
	// empty when the model reaches them some other way, such as a sandbox it
	// browses with its bash tool. The name travels with the skills so the
	// prompt names the tool the agent actually added, rather than one it
	// assumes is there.
	//
	// Like DeferredTools, these are the flattened, JSON-serializable view: the
	// SkillProvider itself holds filesystem handles that do not survive a
	// Temporal activity boundary.
	Skills        []Skill `json:"skills,omitempty"`
	SkillToolName string  `json:"skill_tool_name,omitempty"`
}

type SystemPromptProvider interface {
	GetPrompt(ctx context.Context, data *Dependencies) (string, error)
}
