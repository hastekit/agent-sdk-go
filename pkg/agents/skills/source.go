package skills

import (
	"context"
	"fmt"
	"io/fs"
	"slices"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
)

// Source is a fixed set of developer-owned skills. A Client marks every skill a
// source lists as global: always enabled, and shadowing a user's skill with
// the same name. Give different agents different sources with
// Client.WithGlobalSkills.
//
// Read follows SkillClient.ReadSkill: an empty file returns the SKILL.md body
// without frontmatter, "SKILL.md" returns the original document, and any other
// file must be one of the skill's listed resources. Implementations must be
// safe for concurrent use and honor context cancellation.
type Source interface {
	List(ctx context.Context) ([]agents.Skill, error)
	Read(ctx context.Context, name, file string) (string, error)
}

// fsSource discovers SKILL.md folders and their resources on every listing, so
// skills added or removed on disk appear on the next run without rebuilding the agent.
type fsSource struct {
	load func() (*filesystemCatalog, error)
}

// NewDirSource reads skill folders under directory, including nested folders.
// It validates the initial catalog so a bad path fails at startup.
func NewDirSource(directory string) (Source, error) {
	return newFSSource(func() (*filesystemCatalog, error) { return loadDirectoryCatalog(directory) })
}

// NewFSSource is NewDirSource for an embed.FS or another fs.FS.
// Pass fs.Sub(fsys, "skills") when only that subtree should be exposed.
func NewFSSource(fsys fs.FS) (Source, error) {
	if fsys == nil {
		return nil, fmt.Errorf("skill filesystem is nil")
	}
	return newFSSource(func() (*filesystemCatalog, error) { return loadFSCatalog(fsys) })
}

func newFSSource(load func() (*filesystemCatalog, error)) (Source, error) {
	if _, err := load(); err != nil {
		return nil, err
	}
	return &fsSource{load: load}, nil
}

func (s *fsSource) List(ctx context.Context) ([]agents.Skill, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	catalog, err := s.load()
	if err != nil {
		return nil, err
	}
	return catalog.skillsList(), ctx.Err()
}

func (s *fsSource) Read(ctx context.Context, name, file string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	catalog, err := s.load()
	if err != nil {
		return "", err
	}
	if file == "" {
		return catalog.read(name)
	}
	return catalog.readFile(name, file)
}

// bundleSource serves skills defined in code, validated and copied at construction.
type bundleSource struct {
	skills  []agents.Skill
	bundles map[string]Bundle
}

// NewBundleSource serves inline skill bundles, such as ones built in code or
// loaded by the application from its own configuration.
func NewBundleSource(bundles ...Bundle) (Source, error) {
	s := &bundleSource{bundles: map[string]Bundle{}}
	for _, bundle := range bundles {
		m, err := Validate(bundle)
		if err != nil {
			return nil, err
		}
		if _, exists := s.bundles[m.Name]; exists {
			return nil, fmt.Errorf("%w: duplicate skill %q", ErrInvalid, m.Name)
		}
		files := make(map[string][]byte, len(bundle.Files))
		for path, data := range bundle.Files {
			files[path] = slices.Clone(data)
		}
		s.bundles[m.Name] = Bundle{Files: files}
		s.skills = append(s.skills, agents.Skill{Name: m.Name, Description: m.Description, Resources: m.Resources})
	}
	return s, nil
}

func (s *bundleSource) List(ctx context.Context) ([]agents.Skill, error) {
	skills := slices.Clone(s.skills)
	for i := range skills {
		skills[i].Resources = slices.Clone(skills[i].Resources)
	}
	return skills, ctx.Err()
}

func (s *bundleSource) Read(ctx context.Context, name, file string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	bundle, ok := s.bundles[name]
	if !ok {
		return "", ErrNotFound
	}
	return readBundle(bundle, file)
}

// storeSource serves one Store namespace as developer-owned skills.
type storeSource struct {
	store     Store
	namespace string
}

// NewStoreSource serves the skills saved in one Store namespace as globals, for
// a library the application updates at run time. Populate that namespace only
// through trusted server-side calls; never let users write to it. Uploads,
// replacements and deletions are visible on the next listing.
func NewStoreSource(store Store, namespace string) (Source, error) {
	if store == nil || !validName(namespace) {
		return nil, fmt.Errorf("%w: store and namespace required", ErrInvalid)
	}
	return &storeSource{store: store, namespace: namespace}, nil
}

func (s *storeSource) List(ctx context.Context) ([]agents.Skill, error) {
	listed, err := listStore(ctx, s.store, s.namespace)
	if err != nil {
		return nil, err
	}
	skills := make([]agents.Skill, 0, len(listed))
	for _, m := range listed {
		skills = append(skills, agents.Skill{Name: m.Name, Description: m.Description, Resources: slices.Clone(m.Resources)})
	}
	return skills, nil
}

func (s *storeSource) Read(ctx context.Context, name, file string) (string, error) {
	bundle, err := s.store.Get(ctx, s.namespace, name)
	if err != nil {
		return "", err
	}
	return readBundle(bundle, file)
}

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
