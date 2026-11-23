package skills

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
)

// Client is one agent's skill catalog, read from one or more Stores. It
// mirrors mcpclient.Client.
//
// Each store's empty namespace holds global skills: developer-owned, listed for
// every namespace, and shadowing a user's skill with the same name, so a user
// can never replace a developer's skill. A namespace's own skills are enabled
// unless a run disables them. Global skills can live in the same store as
// users' uploads, or in read-only stores such as NewDirStore:
//
//	store, err := skills.NewFileStore("./data/skills") // users' uploads, shared by every agent
//	builtins, err := skills.NewDirStore("./skills")    // developer-owned globals
//	everything := skills.NewClient(store, builtins)
//	custom := skills.NewClient(skills.Filter(store, picks), skills.Filter(builtins, picks))
type Client struct {
	stores []Store
}

// NewClient reads every store at listing and read time. A skill name must be
// unique among all stores' globals, and among their skills for one namespace.
func NewClient(stores ...Store) *Client {
	return &Client{stores: slices.Clone(stores)}
}

// listScope lists one namespace across every store, ordered by name. A name
// two stores both list is ambiguous; which wins is never implicit.
func (c *Client) listScope(ctx context.Context, namespace string) ([]Metadata, error) {
	var listed []Metadata
	seen := map[string]bool{}
	for _, store := range c.stores {
		if store == nil {
			return nil, errors.New("skill store must not be nil")
		}
		page, err := listStore(ctx, store, namespace)
		if err != nil {
			return nil, err
		}
		for _, m := range page {
			if seen[m.Name] {
				return nil, duplicateSkill(namespace, m.Name)
			}
			seen[m.Name] = true
			listed = append(listed, m)
		}
	}
	sort.Slice(listed, func(i, j int) bool { return listed[i].Name < listed[j].Name })
	return listed, nil
}

// getScope reads one namespace's bundle from whichever store holds it.
func (c *Client) getScope(ctx context.Context, namespace, name string) (Bundle, error) {
	var found *Bundle
	for _, store := range c.stores {
		if store == nil {
			return Bundle{}, errors.New("skill store must not be nil")
		}
		bundle, err := store.Get(ctx, namespace, name)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return Bundle{}, err
		}
		if found != nil {
			return Bundle{}, duplicateSkill(namespace, name)
		}
		found = &bundle
	}
	if found == nil {
		return Bundle{}, ErrNotFound
	}
	return *found, nil
}

func duplicateSkill(namespace, name string) error {
	if namespace == "" {
		return fmt.Errorf("duplicate global skill %q", name)
	}
	return fmt.Errorf("duplicate skill %q in one namespace", name)
}

// ListSkills returns globals first, then the namespace's own skills that no
// global shadows, each group ordered by name. An empty namespace lists globals only.
func (c *Client) ListSkills(ctx context.Context, namespace string, _ map[string]any) ([]agents.Skill, error) {
	globals, err := c.listScope(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("list global skills: %w", err)
	}
	skills := make([]agents.Skill, 0, len(globals))
	shadowed := map[string]bool{}
	for _, m := range globals {
		shadowed[m.Name] = true
		skills = append(skills, agents.Skill{Global: true, Name: m.Name, Description: m.Description, FileLocation: m.FileLocation, Resources: slices.Clone(m.Resources)})
	}
	if namespace == "" {
		return skills, nil
	}

	// A shadowed user skill stays in storage but never reaches the agent.
	users, err := c.listScope(ctx, namespace)
	if err != nil {
		return nil, fmt.Errorf("list user skills: %w", err)
	}
	for _, m := range users {
		if !shadowed[m.Name] {
			skills = append(skills, agents.Skill{Name: m.Name, Description: m.Description, FileLocation: m.FileLocation, Resources: slices.Clone(m.Resources)})
		}
	}
	return skills, nil
}

// ReadSkill resolves by the listing's precedence: a global wins in its entirety,
// and the namespace's own skill is read only when no global has the name.
func (c *Client) ReadSkill(ctx context.Context, namespace string, _ map[string]any, name, file string) (string, error) {
	bundle, err := c.getScope(ctx, "", name)
	if errors.Is(err, ErrNotFound) && namespace != "" {
		bundle, err = c.getScope(ctx, namespace, name)
	}
	if err != nil {
		return "", err
	}
	return readBundle(bundle, file)
}

var _ agents.SkillClient = (*Client)(nil)

// listStore reads every page of one namespace, rejecting cursors that repeat.
func listStore(ctx context.Context, store Store, namespace string) ([]Metadata, error) {
	var result []Metadata
	cursor := ""
	seen := map[string]bool{}
	for {
		page, err := store.List(ctx, namespace, ListOptions{Limit: 200, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		result = append(result, page.Skills...)
		if page.NextCursor == "" {
			return result, nil
		}
		if seen[page.NextCursor] {
			return nil, fmt.Errorf("skill store repeated pagination cursor")
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
	}
}

// readBundle returns instructions without frontmatter for an empty file, and
// any other bundled file, including an explicit SKILL.md, as stored.
func readBundle(bundle Bundle, file string) (string, error) {
	instructions := file == ""
	if instructions {
		file = agents.SkillFileName
	}
	data, ok := bundle.Files[file]
	if !ok {
		return "", ErrNotFound
	}
	if instructions {
		_, body, err := parseSkillFrontmatter(data)
		return body, err
	}
	return string(data), nil
}
