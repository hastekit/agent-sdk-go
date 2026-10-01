package skills

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
)

// Client is one agent's skill catalog: the developer's global skills from its
// Sources plus the skills each namespace saved in the shared Store. It mirrors
// mcpclient.Client.
//
// Globals are always enabled and shadow a user skill with the same name, so a
// user can never replace or turn off a developer's skill. Namespace skills are
// enabled unless a run disables them. Share one Store across agents and give
// each its own globals:
//
//	base := skills.NewClient(userStore)
//	reviewer := base.WithGlobalSkills(reviewSkills)
//	writer := base.WithGlobalSkills(writingSkills, styleGuide)
type Client struct {
	store   Store
	globals []Source
}

// NewClient reads users' own skills from store by namespace. A nil store serves
// globals only. Add developer-owned skills with WithGlobalSkills.
func NewClient(store Store) *Client {
	return &Client{store: store}
}

// WithGlobalSkills returns a copy with additional developer-owned sources. Copies
// share the user store, so every agent sees the same user skills, while each
// agent's globals stay its own. A skill name must be unique across all globals.
func (c *Client) WithGlobalSkills(sources ...Source) *Client {
	return &Client{store: c.store, globals: append(slices.Clone(c.globals), sources...)}
}

// globalCatalog lists every global source and records which one owns each name.
func (c *Client) globalCatalog(ctx context.Context) ([]agents.Skill, map[string]Source, error) {
	var skills []agents.Skill
	owners := map[string]Source{}
	for _, source := range c.globals {
		if source == nil {
			return nil, nil, errors.New("skill source must not be nil")
		}
		listed, err := source.List(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("list global skills: %w", err)
		}
		for _, skill := range listed {
			// Two developer sources claiming one name is a configuration error, not a precedence rule.
			if _, exists := owners[skill.Name]; exists {
				return nil, nil, fmt.Errorf("duplicate global skill %q", skill.Name)
			}
			owners[skill.Name] = source
			skill.Global = true
			skill.Resources = slices.Clone(skill.Resources)
			skills = append(skills, skill)
		}
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	return skills, owners, nil
}

// ListSkills returns globals first, then the namespace's own skills that no
// global shadows, each group ordered by name. An empty namespace lists globals only.
func (c *Client) ListSkills(ctx context.Context, namespace string, _ map[string]any) ([]agents.Skill, error) {
	skills, owners, err := c.globalCatalog(ctx)
	if err != nil {
		return nil, err
	}
	if namespace == "" || c.store == nil {
		return skills, nil
	}

	// A shadowed user skill stays in storage but never reaches the agent.
	listed, err := listStore(ctx, c.store, namespace)
	if err != nil {
		return nil, fmt.Errorf("list user skills: %w", err)
	}
	var users []agents.Skill
	for _, m := range listed {
		if _, global := owners[m.Name]; global {
			continue
		}
		users = append(users, agents.Skill{Name: m.Name, Description: m.Description, Resources: slices.Clone(m.Resources)})
	}
	sort.Slice(users, func(i, j int) bool { return users[i].Name < users[j].Name })
	return append(skills, users...), nil
}

// ReadSkill resolves by the listing's precedence: a global wins in its entirety,
// and the namespace's own skill is read only when no global has the name.
func (c *Client) ReadSkill(ctx context.Context, namespace string, _ map[string]any, name, file string) (string, error) {
	_, owners, err := c.globalCatalog(ctx)
	if err != nil {
		return "", err
	}
	if source, ok := owners[name]; ok {
		return source.Read(ctx, name, file)
	}
	if namespace == "" || c.store == nil {
		return "", ErrNotFound
	}
	bundle, err := c.store.Get(ctx, namespace, name)
	if err != nil {
		return "", err
	}
	return readBundle(bundle, file)
}

var _ agents.SkillClient = (*Client)(nil)
