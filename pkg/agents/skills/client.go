package skills

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
)

// Client is one agent's skill catalog, read from two stores. It mirrors
// mcpclient.Client.
//
// The built-in store holds the developer's skills. They are global: listed for
// every namespace, always on, and shadowing a user's skill with the same name,
// so a user can never replace a developer's skill. The user store, given with
// WithUserStore, holds each namespace's own skills, which a run may turn off
// (agents.SkillSelection). Stores do not know which role they play: the client
// reads the built-in store under the empty namespace and the user store under
// the run's, so any store can fill either, and one store can fill both:
//
//	builtIns, err := skills.NewDirStore("./skills")    // developer-owned globals
//	uploads, err := skills.NewFileStore("./data/skills") // users' uploads, shared by every agent
//	client := skills.NewClient(builtIns, skills.WithUserStore(uploads))
//	custom := skills.NewClient(skills.Filter(builtIns, picks), skills.WithUserStore(skills.Filter(uploads, picks)))
type Client struct {
	builtIn Store
	user    Store
}

// ClientOption configures NewClient.
type ClientOption func(*Client)

// WithUserStore reads each namespace's own skills from store.
func WithUserStore(store Store) ClientOption {
	return func(c *Client) { c.user = store }
}

// NewClient reads its stores at listing and read time, so skills added at run
// time appear on the next run. builtIn may be nil, for a client of users'
// skills only.
func NewClient(builtIn Store, options ...ClientOption) *Client {
	client := &Client{builtIn: builtIn}
	for _, option := range options {
		if option != nil {
			option(client)
		}
	}
	return client
}

// ListSkills returns the built-in skills first, then the namespace's own that
// no built-in shadows, each group ordered by name. An empty namespace lists
// built-ins only.
func (c *Client) ListSkills(ctx context.Context, namespace string, _ map[string]any) ([]agents.Skill, error) {
	globals, err := listStore(ctx, c.builtIn, "")
	if err != nil {
		return nil, fmt.Errorf("list built-in skills: %w", err)
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
	users, err := listStore(ctx, c.user, namespace)
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

// ReadSkill resolves by the listing's precedence: a built-in wins in its
// entirety, and the namespace's own skill is read only when no built-in has
// the name.
func (c *Client) ReadSkill(ctx context.Context, namespace string, _ map[string]any, name, file string) (string, error) {
	bundle, err := getStore(ctx, c.builtIn, "", name)
	if errors.Is(err, ErrNotFound) && namespace != "" {
		bundle, err = getStore(ctx, c.user, namespace, name)
	}
	if err != nil {
		return "", err
	}
	return readBundle(bundle, file)
}

var _ agents.SkillClient = (*Client)(nil)

func getStore(ctx context.Context, store Store, namespace, name string) (Bundle, error) {
	if store == nil {
		return Bundle{}, ErrNotFound
	}
	return store.Get(ctx, namespace, name)
}

// listStore reads every page of one namespace, rejecting cursors that repeat.
// A nil store lists nothing.
func listStore(ctx context.Context, store Store, namespace string) ([]Metadata, error) {
	if store == nil {
		return nil, ctx.Err()
	}
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
