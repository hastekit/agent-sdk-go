package skills

import "context"

// FilterOption configures Filter.
type FilterOption func(*filteredStore)

// KeepUserSkills keeps every namespace-owned skill that Filter does not name,
// for agents that should see whatever skills users upload.
func KeepUserSkills() FilterOption {
	return func(f *filteredStore) { f.keepUsers = true }
}

// filteredStore lists and reads only the chosen skills of another store.
type filteredStore struct {
	store     Store
	names     map[string]bool
	keepUsers bool
}

// Filter returns a store that lists and reads only the named skills of store,
// so an agent can take part of a shared store instead of all of it. Names match
// global and namespace-owned skills alike, like mcpclient.Filter:
//
//	store, err := skills.NewFileStore("./data/skills")       // globals and users' uploads
//	client := skills.NewClient(skills.Filter(store, []string{
//		"release-review",   // a global skill
//		"my-meeting-notes", // the user's own upload
//	}))
//
// A namespace's skills Filter does not name are dropped, including skills the
// user uploads later; pass KeepUserSkills to keep them. A name the store does
// not list is ignored, since stores change at run time, and reading any skill
// left out returns ErrNotFound. No names keep nothing, or only users' skills
// with KeepUserSkills. Filtering a filtered store keeps names both share.
//
// The result is read-only. Give NewHandler the unfiltered MutableStore.
//
// When a global and a user's skill share a kept name, the global wins as usual.
// A global left out is not the agent's at all, so it does not shadow a kept
// user skill with the same name. Per-run SkillSelection.Disable still applies.
func Filter(store Store, names []string, options ...FilterOption) Store {
	if store == nil {
		return nil // reported by the client like any nil store
	}
	filtered := &filteredStore{store: store, names: make(map[string]bool, len(names))}
	for _, name := range names {
		filtered.names[name] = true
	}
	for _, option := range options {
		if option != nil {
			option(filtered)
		}
	}
	return filtered
}

func (f *filteredStore) keeps(namespace, name string) bool {
	return f.names[name] || (namespace != "" && f.keepUsers)
}

func (f *filteredStore) List(ctx context.Context, namespace string, opts ListOptions) (Page, error) {
	page, err := f.store.List(ctx, namespace, opts)
	if err != nil {
		return Page{}, err
	}
	// Keep the cursor: a page this filter empties may still have more after it.
	kept := Page{Skills: []Metadata{}, NextCursor: page.NextCursor}
	for _, m := range page.Skills {
		if f.keeps(namespace, m.Name) {
			kept.Skills = append(kept.Skills, m)
		}
	}
	return kept, nil
}

func (f *filteredStore) Get(ctx context.Context, namespace, name string) (Bundle, error) {
	if !f.keeps(namespace, name) {
		return Bundle{}, ErrNotFound
	}
	return f.store.Get(ctx, namespace, name)
}
