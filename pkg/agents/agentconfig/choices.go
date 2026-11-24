package agentconfig

import (
	"context"
	"sort"

	"github.com/hastekit/agent-sdk-go/pkg/agents/mcpclient"
	"github.com/hastekit/agent-sdk-go/pkg/agents/skills"
)

// Choice is one thing a config may pick, for a UI building one.
type Choice struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// BuiltIn marks what the developer owns — a global skill or connector, a
	// built-in agent — as opposed to the namespace's own.
	BuiltIn bool `json:"builtIn,omitempty"`
}

// Choices is what a namespace's configs may pick from.
type Choices struct {
	// Models are the allowlisted model ids. Empty when the ModelResolver
	// cannot list them (see ModelLister); any id is then checked on save.
	Models []string `json:"models"`
	// Tools are the catalog's tools, by catalog key.
	Tools []Choice `json:"tools"`
	// Skills are the global skills and the namespace's own.
	Skills []Choice `json:"skills"`
	// Connectors are the MCP servers the namespace can pick, global or its own.
	Connectors []Choice `json:"connectors"`
	// Agents are the handoff targets and sub-agents the namespace can name.
	Agents []Choice `json:"agents"`
}

func emptyChoices() Choices {
	return Choices{Models: []string{}, Tools: []Choice{}, Skills: []Choice{}, Connectors: []Choice{}, Agents: []Choice{}}
}

// ModelLister is a ModelResolver that can say which ids it allows, so a UI can
// offer them. Models and AllowModels implement it.
type ModelLister interface {
	ModelIDs() []string
}

func (m Models) ModelIDs() []string {
	ids := make([]string, 0, len(m))
	for id, provider := range m {
		if provider != nil {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func (m *allowedModels) ModelIDs() []string {
	ids := make([]string, 0, len(m.allowed))
	for id := range m.allowed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Choices lists what namespace's configs may pick from the Builder's
// dependencies. Agents is left for Registry.Choices, which knows the code
// agents too. Nothing is built or connected: connectors are listed from their
// stores, without their tools.
func (b *Builder) Choices(ctx context.Context, namespace string) (Choices, error) {
	choices := emptyChoices()
	if lister, ok := b.deps.Models.(ModelLister); ok {
		choices.Models = append(choices.Models, lister.ModelIDs()...)
	}
	for _, key := range b.ToolNames() {
		choice := Choice{Name: key, BuiltIn: true}
		if function := b.deps.Tools[key].GetToolDescriptor().ToolUnion.OfFunction; function != nil && function.Description != nil {
			choice.Description = *function.Description
		}
		choices.Tools = append(choices.Tools, choice)
	}
	if b.deps.BuiltInSkillStore != nil || b.deps.UserSkillStore != nil {
		listed, err := skills.NewClient(b.deps.BuiltInSkillStore, skills.WithUserStore(b.deps.UserSkillStore)).ListSkills(ctx, namespace, nil)
		if err != nil {
			return Choices{}, err
		}
		for _, skill := range listed {
			choices.Skills = append(choices.Skills, Choice{Name: skill.Name, Description: skill.Description, BuiltIn: skill.Global})
		}
	}
	// Built-ins first, so one shadows a user's server with its name.
	seen := map[string]bool{}
	for _, source := range []struct {
		store     mcpclient.Store
		namespace string
		user      bool
	}{{b.deps.BuiltInMCPStore, "", false}, {b.deps.UserMCPStore, namespace, true}} {
		if source.store == nil || (source.user && namespace == "") {
			continue
		}
		configs, err := source.store.ListServerConfigs(ctx, source.namespace, nil)
		if err != nil {
			return Choices{}, err
		}
		for _, config := range configs {
			if !seen[config.Name] {
				seen[config.Name] = true
				choices.Connectors = append(choices.Connectors, Choice{Name: config.Name, BuiltIn: !source.user})
			}
		}
	}
	sort.Slice(choices.Connectors, func(i, j int) bool { return choices.Connectors[i].Name < choices.Connectors[j].Name })
	return choices, nil
}

// Choices is the Builder's choices plus every agent namespace can hand off to:
// built-ins and the namespace's own. Code agents are not among them, since a
// durable run carries its graph as configs.
func (r *Registry) Choices(ctx context.Context, namespace string) (Choices, error) {
	choices, err := r.builder.Choices(ctx, namespace)
	if err != nil {
		return Choices{}, err
	}
	listed, err := r.list(ctx, namespace)
	if err != nil {
		return Choices{}, err
	}
	// list leaves out a namespace's config a built-in shadows, so no name
	// repeats.
	for _, cfg := range listed.builtIn {
		choices.Agents = append(choices.Agents, Choice{Name: cfg.Name, Description: cfg.Description, BuiltIn: true})
	}
	for _, cfg := range listed.own {
		choices.Agents = append(choices.Agents, Choice{Name: cfg.Name, Description: cfg.Description})
	}
	return choices, nil
}
