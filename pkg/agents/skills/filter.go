package skills

import "context"

// filteredStore lists and reads only the chosen skills of another store.
type filteredStore struct {
	store Store
	names map[string]bool
}

// Filter returns a store that lists and reads only the named skills of store,
// in every namespace, so an agent can take part of a store instead of all of
// it. Filter either of a Client's stores, or both:
//
//	client := skills.NewClient(
//		skills.Filter(builtIns, []string{"release-review"}),                     // a developer's skill
//		skills.WithUserStore(skills.Filter(uploads, []string{"my-meeting-notes"})), // the user's own upload
//	)
//
// A name the store does not list is ignored, since stores change at run time,
// and reading any skill left out returns ErrNotFound. No names keep nothing.
// Filtering a filtered store keeps names both share.
//
// The result is read-only. Give NewHandler the unfiltered MutableStore.
func Filter(store Store, names []string) Store {
	if store == nil {
		return nil
	}
	filtered := &filteredStore{store: store, names: make(map[string]bool, len(names))}
	for _, name := range names {
		filtered.names[name] = true
	}
	return filtered
}

func (f *filteredStore) List(ctx context.Context, namespace string, opts ListOptions) (Page, error) {
	page, err := f.store.List(ctx, namespace, opts)
	if err != nil {
		return Page{}, err
	}
	// Keep the cursor: a page this filter empties may still have more after it.
	kept := Page{Skills: []Metadata{}, NextCursor: page.NextCursor}
	for _, m := range page.Skills {
		if f.names[m.Name] {
			kept.Skills = append(kept.Skills, m)
		}
	}
	return kept, nil
}

func (f *filteredStore) Get(ctx context.Context, namespace, name string) (Bundle, error) {
	if !f.names[name] {
		return Bundle{}, ErrNotFound
	}
	return f.store.Get(ctx, namespace, name)
}
