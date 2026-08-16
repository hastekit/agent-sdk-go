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

	// Skills the agent was given, and the hint introducing them — what they
	// are and how to read one. Both come from the SkillProvider, the only
	// thing that knows how its skills are actually reached. A provider that
	// says nothing gets no prose at all, just the catalogue: guessing here
	// would sooner or later name a tool the agent does not have.
	//
	// Like DeferredTools, these are the flattened, JSON-serializable view: the
	// SkillProvider itself holds filesystem handles that do not survive a
	// Temporal activity boundary.
	Skills    []Skill `json:"skills,omitempty"`
	SkillHint string  `json:"skill_hint,omitempty"`
}

type SystemPromptProvider interface {
	GetPrompt(ctx context.Context, data *Dependencies) (string, error)
}
