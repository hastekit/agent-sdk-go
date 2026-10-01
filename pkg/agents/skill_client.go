package agents

import (
	"context"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"
)

// SkillSelection disables user-owned skills by name for one run. Global skills
// cannot be disabled; every other visible skill is enabled unless named here.
// Unknown names are ignored so one selection can pass through agents with
// different catalogs during handoffs.
type SkillSelection struct {
	Disable []string `json:"disable,omitempty"`
}

// SkillClient owns an agent's skill catalog: the developer's global skills plus
// the skills a namespace added itself. It mirrors MCPClient.
//
// ListSkills returns every skill visible to the namespace, globals marked
// Global. A global shadows a user skill with the same name, so names are
// unique. An empty namespace lists globals only. ReadSkill resolves by the same
// precedence: a request for instructions passes an empty file and receives the
// SKILL.md body without frontmatter; "SKILL.md" returns the original document.
// Namespace is authoritative and passed explicitly; RunContext is application
// data. Implementations must be safe for concurrent use.
type SkillClient interface {
	ListSkills(ctx context.Context, namespace string, runContext map[string]any) ([]Skill, error)
	ReadSkill(ctx context.Context, namespace string, runContext map[string]any, name, file string) (string, error)
}

// ListedSkill is a catalog entry for clients building a skill picker. Name can
// be copied directly into SkillSelection. Enabled describes the supplied
// selection; a Global entry is always enabled.
type ListedSkill struct {
	Skill
	Enabled bool `json:"enabled"`
}

func validSkillPart(name string) bool {
	return name != "" && name == strings.TrimSpace(name) && !strings.ContainsAny(name, "/\\") && name != "." && name != ".."
}

// skillEnabled is the one place selection policy lives, so no client can let a
// user turn off a developer's skill.
func skillEnabled(skill Skill, selection SkillSelection) bool {
	return skill.Global || !slices.Contains(selection.Disable, skill.Name)
}

// listSkillCatalog validates the client's listing and applies the selection.
// It returns the full catalog and the enabled skills by name.
func (e *Agent) listSkillCatalog(ctx context.Context, namespace string, rc map[string]any, selection SkillSelection) ([]ListedSkill, map[string]Skill, error) {
	if e.skillClient == nil {
		return nil, nil, nil
	}
	skills, err := e.skillClient.ListSkills(ctx, namespace, rc)
	if err != nil {
		return nil, nil, fmt.Errorf("list skills: %w", err)
	}

	// The listing is the trusted allowlist for read_skill, so reject anything ambiguous.
	catalog := make([]ListedSkill, 0, len(skills))
	enabled := map[string]Skill{}
	seen := map[string]bool{}
	for _, skill := range skills {
		if !validSkillPart(skill.Name) {
			return nil, nil, fmt.Errorf("invalid skill name %q", skill.Name)
		}
		if seen[skill.Name] {
			return nil, nil, fmt.Errorf("duplicate skill %q", skill.Name)
		}
		seen[skill.Name] = true
		skill.Resources = slices.Clone(skill.Resources)
		for _, file := range skill.Resources {
			if !fs.ValidPath(file) || strings.Contains(file, "\\") {
				return nil, nil, fmt.Errorf("skill %q has invalid resource %q", skill.Name, file)
			}
		}
		listed := ListedSkill{Skill: skill, Enabled: skillEnabled(skill, selection)}
		catalog = append(catalog, listed)
		if listed.Enabled {
			enabled[skill.Name] = skill
		}
	}
	return catalog, enabled, nil
}

// ListSkills returns the current catalog for a UI or other caller. A run lists
// again using its own context; this result is not a reservation or authorization.
func (e *Agent) ListSkills(ctx context.Context, namespace string, runContext map[string]any, selection SkillSelection) ([]ListedSkill, error) {
	catalog, _, err := e.listSkillCatalog(ctx, namespace, runContext, selection)
	return catalog, err
}

// prepareSkills builds one reader from the run's enabled catalog without
// mutating shared configuration. Durable clients provide their own proxy.
func (e *Agent) prepareSkills(ctx context.Context, in *AgentInput, tools []Tool) ([]Tool, []Skill, string, error) {
	if e.skillClient == nil {
		return tools, nil, "", nil
	}
	for _, tool := range tools {
		if functionName(tool) == ReadSkillToolName {
			return nil, nil, "", fmt.Errorf("read_skill is reserved when using skills")
		}
	}
	runContext := maps.Clone(in.RunContext)
	if runContext == nil {
		runContext = map[string]any{}
	}
	catalog, enabled, err := e.listSkillCatalog(ctx, in.Namespace, runContext, in.Skills)
	if err != nil {
		return nil, nil, "", err
	}
	if len(enabled) == 0 {
		return tools, nil, "", nil
	}
	var skills []Skill
	for _, listed := range catalog {
		if listed.Enabled {
			skills = append(skills, listed.Skill)
		}
	}
	reader := &resolvedSkillTool{BaseTool: dynamicSkillDescriptor(), client: e.skillClient, skills: enabled, namespace: in.Namespace, runContext: runContext}
	return append(slices.Clone(tools), reader), skills, "Read enabled skills using read_skill with the exact name listed. Pass file to read a bundled resource.", nil
}
