package agents

import (
	"context"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"
)

// SkillPolicy controls whether a skill is available to a run. The zero value
// is opt-in: adding a remote skill never silently enables it.
type SkillPolicy string

const (
	SkillOptIn    SkillPolicy = "opt_in"
	SkillRequired SkillPolicy = "required"
	SkillEnabled  SkillPolicy = "enabled"
	SkillBlocked  SkillPolicy = "blocked"
)

// SkillSelection identifies skills by their plain names. Required skills cannot be disabled;
// blocked skills cannot be enabled. Unknown selections are ignored so the same
// input can pass through agents with different skill sets during handoffs.
type SkillSelection struct {
	Enable  []string `json:"enable,omitempty"`
	Disable []string `json:"disable,omitempty"`
}

// SkillSet lists metadata once per run and resolves content only when requested.
// Namespace is passed explicitly from AgentInput; RunContext is application data.
// Implementations must be safe for concurrent use and honor context cancellation.
// Listing defines the policy and resource allowlist trusted by the agent. Never
// derive policy from untrusted input. Resolve receives the skill name and an
// empty file when reading instructions. Later entries in AgentConfig.Skills
// replace earlier skills with the same name, including policy and resources.
type SkillSet interface {
	GetName() string
	ListSkills(ctx context.Context, namespace string, runContext map[string]any) ([]Skill, error)
	ResolveSkill(ctx context.Context, namespace string, runContext map[string]any, name, file string) (string, error)
}

// SkillSetFuncs implements SkillSet using application callbacks. List may read
// a database or registry; Resolve may read embedded, filesystem or HTTP content.
// The application owns authentication, tenant filtering, and content validation.
type SkillSetFuncs struct {
	Name    string
	List    func(context.Context, string, map[string]any) ([]Skill, error)
	Resolve func(context.Context, string, map[string]any, string, string) (string, error)
}

func (s SkillSetFuncs) GetName() string { return s.Name }
func (s SkillSetFuncs) ListSkills(ctx context.Context, namespace string, rc map[string]any) ([]Skill, error) {
	if s.List == nil {
		return nil, fmt.Errorf("skill set %q has no list function", s.Name)
	}
	return s.List(ctx, namespace, rc)
}
func (s SkillSetFuncs) ResolveSkill(ctx context.Context, namespace string, rc map[string]any, name, file string) (string, error) {
	if s.Resolve == nil {
		return "", fmt.Errorf("skill set %q has no resolver", s.Name)
	}
	return s.Resolve(ctx, namespace, rc, name, file)
}

// ListedSkill is a catalog entry for clients building a skill picker. Name is
// unprefixed and can be copied directly into SkillSelection. Blocked entries
// are omitted. Enabled describes the supplied selection, including policy.
type ListedSkill struct {
	Skill
	Enabled bool `json:"enabled"`
}

type skillBinding struct {
	skill        Skill
	originalName string
	set          SkillSet
}

func validSkillPart(name string) bool {
	return name != "" && name == strings.TrimSpace(name) && !strings.ContainsAny(name, "/\\") && name != "." && name != ".."
}

func skillEnabled(policy SkillPolicy, name string, selection SkillSelection) (bool, error) {
	switch policy {
	case SkillRequired:
		return true, nil
	case SkillBlocked:
		return false, nil
	case SkillEnabled:
		return !slices.Contains(selection.Disable, name), nil
	case SkillOptIn, "":
		return slices.Contains(selection.Enable, name) && !slices.Contains(selection.Disable, name), nil
	default:
		return false, fmt.Errorf("skill %q has invalid policy %q", name, policy)
	}
}

func (e *Agent) listSkillBindings(ctx context.Context, namespace string, rc map[string]any, selection SkillSelection) ([]ListedSkill, map[string]skillBinding, error) {
	if strings.TrimSpace(namespace) == "" {
		namespace = "default"
	}
	var catalog []ListedSkill
	bindings := map[string]skillBinding{}
	merged := map[string]skillBinding{}
	var names []string
	if err := ValidateSkillSets(e.skillSets); err != nil {
		return nil, nil, err
	}
	for _, set := range e.skillSets {
		skills, err := set.ListSkills(ctx, namespace, rc)
		if err != nil {
			return nil, nil, fmt.Errorf("list skill set %q: %w", set.GetName(), err)
		}
		for _, skill := range skills {
			if !validSkillPart(skill.Name) {
				return nil, nil, fmt.Errorf("invalid skill name %q in set %q", skill.Name, set.GetName())
			}
			skill.Resources = slices.Clone(skill.Resources)
			for _, file := range skill.Resources {
				if !fs.ValidPath(file) || strings.Contains(file, "\\") {
					return nil, nil, fmt.Errorf("skill %q has invalid resource %q", skill.Name, file)
				}
			}
			_, err := skillEnabled(skill.Policy, skill.Name, selection)
			if err != nil {
				return nil, nil, err
			}
			if _, exists := merged[skill.Name]; !exists {
				names = append(names, skill.Name)
			}
			merged[skill.Name] = skillBinding{skill, skill.Name, set}
		}
	}
	// Apply selection only after merging so a disabled or blocked replacement
	// cannot leave an earlier source's reader accessible.
	for _, name := range names {
		binding := merged[name]
		skill := binding.skill
		if skill.Policy == SkillBlocked {
			continue
		}
		enabled, _ := skillEnabled(skill.Policy, name, selection) // validated above
		catalog = append(catalog, ListedSkill{Skill: skill, Enabled: enabled})
		if enabled {
			bindings[name] = binding
		}
	}
	return catalog, bindings, nil
}

// ListSkills returns the current catalog for a UI or other caller. A run lists
// again using its own context; this result is not a reservation or authorization.
// An empty namespace defaults to "default".
func (e *Agent) ListSkills(ctx context.Context, namespace string, runContext map[string]any, selection SkillSelection) ([]ListedSkill, error) {
	catalog, _, err := e.listSkillBindings(ctx, namespace, runContext, selection)
	return catalog, err
}

// prepareSkills builds one reader from the run's enabled catalog without
// mutating shared configuration. Durable sets provide their own resolver proxy.
func (e *Agent) prepareSkills(ctx context.Context, in *AgentInput, tools []Tool) ([]Tool, []Skill, string, error) {
	if len(e.skillSets) == 0 {
		return tools, nil, "", nil
	}
	for _, tool := range tools {
		if functionName(tool) == ReadSkillToolName {
			return nil, nil, "", fmt.Errorf("read_skill is reserved when using Skills")
		}
	}
	runContext := maps.Clone(in.RunContext)
	if runContext == nil {
		runContext = map[string]any{}
	}
	namespace := in.Namespace
	if strings.TrimSpace(namespace) == "" {
		namespace = "default"
	}
	catalog, bindings, err := e.listSkillBindings(ctx, namespace, runContext, in.Skills)
	if err != nil {
		return nil, nil, "", err
	}
	var skills []Skill
	for _, listed := range catalog {
		if listed.Enabled {
			skills = append(skills, listed.Skill)
		}
	}
	if len(bindings) == 0 {
		return tools, nil, "", nil
	}
	reader := &resolvedSkillTool{BaseTool: dynamicSkillDescriptor(), bindings: bindings, namespace: namespace, runContext: runContext}
	return append(slices.Clone(tools), reader), skills, "Read enabled skills using read_skill with the exact name listed. Pass file to read a bundled resource.", nil
}
