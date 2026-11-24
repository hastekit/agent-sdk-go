package skills

import (
	"context"
	"fmt"
	"io/fs"
	"slices"
	"sort"
)

// fsStore serves skill folders, the same for every namespace. It rediscovers
// them on every call, so skills added or removed on disk appear on the next
// run without rebuilding the agent.
type fsStore struct {
	load func() (*filesystemCatalog, error)
}

// NewDirStore serves the skill folders under directory, including nested
// folders, under every namespace: as a Client's built-in store they are
// global skills, as its user store they are every user's own. It validates the
// folders at construction so a bad path fails at startup, and never accepts
// writes.
func NewDirStore(directory string) (Store, error) {
	return newFSStore(func() (*filesystemCatalog, error) { return loadDirectoryCatalog(directory) })
}

// NewFSStore is NewDirStore for an embed.FS or another fs.FS.
// Pass fs.Sub(fsys, "skills") when only that subtree should be exposed.
func NewFSStore(fsys fs.FS) (Store, error) {
	if fsys == nil {
		return nil, fmt.Errorf("skill filesystem is nil")
	}
	return newFSStore(func() (*filesystemCatalog, error) { return loadFSCatalog(fsys) })
}

func newFSStore(load func() (*filesystemCatalog, error)) (Store, error) {
	if _, err := load(); err != nil {
		return nil, err
	}
	return &fsStore{load: load}, nil
}

func (s *fsStore) List(ctx context.Context, _ string, opts ListOptions) (Page, error) {
	if err := ctx.Err(); err != nil {
		return Page{}, err
	}
	catalog, err := s.load()
	if err != nil {
		return Page{}, err
	}
	var listed []Metadata
	for _, skill := range catalog.skillsList() {
		listed = append(listed, Metadata{Name: skill.Name, Description: skill.Description, Resources: skill.Resources, FileLocation: skill.FileLocation})
	}
	return pageOf(listed, opts), ctx.Err()
}

func (s *fsStore) Get(ctx context.Context, _ string, name string) (Bundle, error) {
	if err := ctx.Err(); err != nil {
		return Bundle{}, err
	}
	catalog, err := s.load()
	if err != nil {
		return Bundle{}, err
	}
	return catalog.bundle(name)
}

// inlineStore serves skills defined in code, validated and copied at construction.
type inlineStore struct {
	listed  []Metadata
	bundles map[string]Bundle
}

// NewInlineStore serves skill bundles defined in code, or loaded by the
// application from its own configuration, under every namespace, as
// NewDirStore does. It validates and copies them, and never accepts writes.
func NewInlineStore(bundles ...Bundle) (Store, error) {
	store := &inlineStore{bundles: map[string]Bundle{}}
	for _, bundle := range bundles {
		m, err := Validate(bundle)
		if err != nil {
			return nil, err
		}
		if _, exists := store.bundles[m.Name]; exists {
			return nil, fmt.Errorf("%w: duplicate skill %q", ErrInvalid, m.Name)
		}
		store.bundles[m.Name] = cloneBundle(bundle)
		store.listed = append(store.listed, m)
	}
	return store, nil
}

func (s *inlineStore) List(ctx context.Context, _ string, opts ListOptions) (Page, error) {
	return pageOf(s.listed, opts), ctx.Err()
}

func (s *inlineStore) Get(ctx context.Context, _ string, name string) (Bundle, error) {
	if err := ctx.Err(); err != nil {
		return Bundle{}, err
	}
	bundle, ok := s.bundles[name]
	if !ok {
		return Bundle{}, ErrNotFound
	}
	return cloneBundle(bundle), nil
}

// pageOf pages through an in-memory listing in name order. The cursor is the
// last name returned, so it stays valid while skills are added or removed.
func pageOf(listed []Metadata, opts ListOptions) Page {
	sorted := slices.Clone(listed)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	page := Page{Skills: []Metadata{}}
	for _, m := range sorted {
		if opts.Cursor != "" && m.Name <= opts.Cursor {
			continue
		}
		if len(page.Skills) == limit(opts.Limit) {
			page.NextCursor = page.Skills[len(page.Skills)-1].Name
			break
		}
		m.Resources = slices.Clone(m.Resources)
		page.Skills = append(page.Skills, m)
	}
	return page
}

func cloneBundle(bundle Bundle) Bundle {
	files := make(map[string][]byte, len(bundle.Files))
	for path, data := range bundle.Files {
		files[path] = slices.Clone(data)
	}
	return Bundle{Files: files}
}
