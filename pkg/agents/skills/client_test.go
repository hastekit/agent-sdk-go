package skills

import (
	"context"
	"fmt"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/stretchr/testify/require"
)

func testInlineStore(t *testing.T, bundles ...Bundle) Store {
	t.Helper()
	store, err := NewInlineStore(bundles...)
	require.NoError(t, err)
	return store
}

func skillNames(t *testing.T, client *Client, namespace string) map[string]bool {
	t.Helper()
	listed, err := client.ListSkills(t.Context(), namespace, nil)
	require.NoError(t, err)
	result := map[string]bool{}
	for _, skill := range listed {
		result[skill.Name] = skill.Global
	}
	return result
}

func TestClientGlobalsShadowUserSkills(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			for _, b := range []Bundle{bundle("review", "user instructions"), bundle("notes", "user notes")} {
				b.Files["user-only.md"] = []byte("user resource")
				_, err := store.Put(ctx, "tenant", b)
				require.NoError(t, err)
			}
			global := bundle("review", "developer instructions")
			global.Files["global-only.md"] = []byte("global resource")
			client := NewClient(testInlineStore(t, global), WithUserStore(store))

			// Globals come first; the user's same-named skill never reaches the agent.
			listed, err := client.ListSkills(ctx, "tenant", nil)
			require.NoError(t, err)
			require.Len(t, listed, 2)
			require.Equal(t, "review", listed[0].Name)
			require.True(t, listed[0].Global)
			require.Contains(t, listed[0].Resources, "global-only.md")
			require.Equal(t, "notes", listed[1].Name)
			require.False(t, listed[1].Global)

			// Reads follow the same precedence, and a global never falls back to the user's bundle.
			content, err := client.ReadSkill(ctx, "tenant", nil, "review", "")
			require.NoError(t, err)
			require.Contains(t, content, "developer instructions")
			content, err = client.ReadSkill(ctx, "tenant", nil, "review", "global-only.md")
			require.NoError(t, err)
			require.Equal(t, "global resource", content)
			_, err = client.ReadSkill(ctx, "tenant", nil, "review", "user-only.md")
			require.ErrorIs(t, err, ErrNotFound)
			content, err = client.ReadSkill(ctx, "tenant", nil, "notes", "")
			require.NoError(t, err)
			require.Contains(t, content, "user notes")
			_, err = client.ReadSkill(ctx, "tenant", nil, "missing", "")
			require.ErrorIs(t, err, ErrNotFound)

			// An empty namespace sees globals only.
			listed, err = client.ListSkills(ctx, "", nil)
			require.NoError(t, err)
			require.Len(t, listed, 1)
			_, err = client.ReadSkill(ctx, "", nil, "notes", "")
			require.ErrorIs(t, err, ErrNotFound)

			// Users cannot turn a global off; their own skills are on unless disabled.
			agent := agents.NewAgent(&agents.AgentOptions{Name: "test", SkillClient: client})
			catalog, err := agent.ListSkills(ctx, "tenant", nil, agents.SkillSelection{Disable: []string{"review", "notes"}})
			require.NoError(t, err)
			require.Equal(t, []agents.ListedSkill{
				{Skill: listed[0], Enabled: true},
				{Skill: agents.Skill{Name: "notes", Description: "Review releases", Resources: []string{"refs/check.md", "user-only.md"}}, Enabled: false},
			}, catalog)

			// Without the global, the user's own copy is visible and optional.
			catalog, err = agents.NewAgent(&agents.AgentOptions{Name: "plain", SkillClient: NewClient(nil, WithUserStore(store))}).ListSkills(ctx, "tenant", nil, agents.SkillSelection{Disable: []string{"review"}})
			require.NoError(t, err)
			require.Len(t, catalog, 2)
			require.Equal(t, "notes", catalog[0].Name)
			require.Equal(t, "review", catalog[1].Name)
			require.False(t, catalog[1].Global)
			require.False(t, catalog[1].Enabled)
		})
	}
}

// A store does not know its role, so one store can fill both: the client
// reads its built-ins under the empty namespace.
func TestOneStoreCanFillBothRoles(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			_, err := store.Put(ctx, "", bundle("shared", "shared instructions"))
			require.NoError(t, err)
			_, err = store.Put(ctx, "tenant", bundle("mine", "mine"))
			require.NoError(t, err)

			client := NewClient(store, WithUserStore(store))
			require.Equal(t, map[string]bool{"shared": true, "mine": false}, skillNames(t, client, "tenant"))
			require.Equal(t, map[string]bool{"shared": true}, skillNames(t, client, "other"))
			content, err := client.ReadSkill(ctx, "other", nil, "shared", "refs/check.md")
			require.NoError(t, err)
			require.Equal(t, "check", content)

			// Later saves to the global namespace are visible on the next listing.
			_, err = store.Put(ctx, "", bundle("added", "added later"))
			require.NoError(t, err)
			require.Len(t, skillNames(t, client, "other"), 2)

			// A blank namespace cannot pass for the global one.
			_, err = store.Put(ctx, " ", bundle("sneaky", "sneaky"))
			require.ErrorIs(t, err, ErrInvalid)
			_, err = store.List(ctx, "\t", ListOptions{})
			require.ErrorIs(t, err, ErrInvalid)
		})
	}
}

func TestClientGivesEachAgentItsOwnGlobals(t *testing.T) {
	ctx := t.Context()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	reviewer := NewClient(testInlineStore(t, bundle("review", "review")), WithUserStore(store))
	writer := NewClient(testInlineStore(t, bundle("style", "style")), WithUserStore(store))
	both := NewClient(testInlineStore(t, bundle("review", "review"), bundle("style", "style")), WithUserStore(store))

	// Every client shares the user store; globals stay per client.
	_, err = store.Put(ctx, "tenant", bundle("mine", "user"))
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"mine": false}, skillNames(t, NewClient(nil, WithUserStore(store)), "tenant"))
	require.Equal(t, map[string]bool{"review": true, "mine": false}, skillNames(t, reviewer, "tenant"))
	require.Equal(t, map[string]bool{"style": true, "mine": false}, skillNames(t, writer, "tenant"))
	require.Equal(t, map[string]bool{"review": true, "style": true, "mine": false}, skillNames(t, both, "tenant"))
}

func TestFilterPicksGlobalAndUserSkills(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			for _, global := range []string{"review", "style", "tone"} {
				_, err := store.Put(ctx, "", bundle(global, "developer "+global))
				require.NoError(t, err)
			}
			for _, user := range []string{"notes", "drafts", "review"} {
				_, err := store.Put(ctx, "tenant", bundle(user, "user "+user))
				require.NoError(t, err)
			}
			names := []string{"review", "tone", "notes", "missing"}
			picked := NewClient(Filter(store, names), WithUserStore(Filter(store, names)))
			names[0] = "style" // later changes to the caller's slice do not apply

			// Picks of either kind are kept; the global review still wins over the user's.
			require.Equal(t, map[string]bool{"review": true, "tone": true, "notes": false}, skillNames(t, picked, "tenant"))
			require.Equal(t, map[string]bool{"review": true, "tone": true}, skillNames(t, picked, ""))
			content, err := picked.ReadSkill(ctx, "tenant", nil, "review", "")
			require.NoError(t, err)
			require.Contains(t, content, "developer review")
			content, err = picked.ReadSkill(ctx, "tenant", nil, "notes", "")
			require.NoError(t, err)
			require.Contains(t, content, "user notes")
			for _, name := range []string{"style", "drafts", "missing"} {
				_, err = picked.ReadSkill(ctx, "tenant", nil, name, "")
				require.ErrorIs(t, err, ErrNotFound, name)
			}

			// Later uploads stay out unless picked; the unfiltered store is unchanged.
			_, err = store.Put(ctx, "tenant", bundle("later", "user later"))
			require.NoError(t, err)
			require.NotContains(t, skillNames(t, picked, "tenant"), "later")
			require.Len(t, skillNames(t, NewClient(store, WithUserStore(store)), "tenant"), 6)

			// Each store is filtered on its own; a built-in left out does not shadow the user's.
			split := NewClient(Filter(store, []string{"tone"}), WithUserStore(Filter(store, []string{"review"})))
			require.Equal(t, map[string]bool{"tone": true, "review": false}, skillNames(t, split, "tenant"))
			content, err = split.ReadSkill(ctx, "tenant", nil, "review", "")
			require.NoError(t, err)
			require.Contains(t, content, "user review")
			_, err = split.ReadSkill(ctx, "tenant", nil, "style", "")
			require.ErrorIs(t, err, ErrNotFound)

			// Filters nest as an intersection, and no names keep nothing.
			nested := Filter(Filter(store, []string{"review", "notes"}), []string{"notes", "style"})
			require.Equal(t, map[string]bool{"notes": false}, skillNames(t, NewClient(nested, WithUserStore(nested)), "tenant"))
			none := Filter(store, nil)
			require.Empty(t, skillNames(t, NewClient(none, WithUserStore(none)), "tenant"))

			// Per-run selection still applies to the picks.
			agent := agents.NewAgent(&agents.AgentOptions{Name: "custom", SkillClient: picked})
			catalog, err := agent.ListSkills(ctx, "tenant", nil, agents.SkillSelection{Disable: []string{"notes", "review"}})
			require.NoError(t, err)
			enabled := map[string]bool{}
			for _, skill := range catalog {
				enabled[skill.Name] = skill.Enabled
			}
			require.Equal(t, map[string]bool{"review": true, "tone": true, "notes": false}, enabled)
		})
	}
	// A missing store lists nothing.
	listed, err := NewClient(Filter(nil, []string{"review"})).ListSkills(t.Context(), "tenant", nil)
	require.NoError(t, err)
	require.Empty(t, listed)
}

func TestFilterKeepsPaginating(t *testing.T) {
	// A page the filter empties must not end the listing early.
	store := pagedStore{list: func(_ context.Context, ns string, opts ListOptions) (Page, error) {
		if ns != "tenant" {
			return Page{}, nil
		}
		if opts.Cursor == "" {
			return Page{Skills: []Metadata{{Name: "dropped"}}, NextCursor: "next"}, nil
		}
		return Page{Skills: []Metadata{{Name: "kept"}}}, nil
	}}
	require.Equal(t, map[string]bool{"kept": false}, skillNames(t, NewClient(nil, WithUserStore(Filter(store, []string{"kept"}))), "tenant"))
}

func TestClientWithoutStoresListsNothing(t *testing.T) {
	listed, err := NewClient(nil).ListSkills(t.Context(), "tenant", nil)
	require.NoError(t, err)
	require.Empty(t, listed)
	_, err = NewClient(nil).ReadSkill(t.Context(), "tenant", nil, "review", "")
	require.ErrorIs(t, err, ErrNotFound)

	// A store holds a name once.
	_, err = NewInlineStore(bundle("review", "a"), bundle("review", "b"))
	require.ErrorIs(t, err, ErrInvalid)
}

type pagedStore struct {
	list func(context.Context, string, ListOptions) (Page, error)
	get  func(context.Context, string, string) (Bundle, error)
}

func (s pagedStore) List(ctx context.Context, ns string, opts ListOptions) (Page, error) {
	return s.list(ctx, ns, opts)
}
func (s pagedStore) Get(ctx context.Context, ns, name string) (Bundle, error) {
	if s.get == nil {
		return Bundle{}, ErrNotFound
	}
	return s.get(ctx, ns, name)
}

func TestClientPaginatesEachScope(t *testing.T) {
	var calls []string
	store := pagedStore{list: func(_ context.Context, ns string, opts ListOptions) (Page, error) {
		calls = append(calls, ns+":"+opts.Cursor)
		if opts.Cursor == "" {
			return Page{Skills: []Metadata{{Name: ns + "one"}}, NextCursor: "next"}, nil
		}
		return Page{Skills: []Metadata{{Name: ns + "two"}}}, nil
	}}
	require.Equal(t, map[string]bool{"one": true, "two": true, "tenantone": false, "tenanttwo": false}, skillNames(t, NewClient(store, WithUserStore(store)), "tenant"))
	require.Equal(t, []string{":", ":next", "tenant:", "tenant:next"}, calls)

	looping := pagedStore{list: func(context.Context, string, ListOptions) (Page, error) {
		return Page{NextCursor: "same"}, nil
	}}
	_, err := NewClient(looping).ListSkills(t.Context(), "tenant", nil)
	require.ErrorContains(t, err, "repeated pagination cursor")
}

func TestClientDoesNotHideStoreErrors(t *testing.T) {
	failure := fmt.Errorf("storage unavailable")
	store := pagedStore{
		list: func(context.Context, string, ListOptions) (Page, error) { return Page{}, failure },
		get:  func(context.Context, string, string) (Bundle, error) { return Bundle{}, failure },
	}

	// A failing store fails listing and reads rather than silently dropping skills.
	client := NewClient(testInlineStore(t, bundle("review", "review")), WithUserStore(store))
	_, err := client.ListSkills(t.Context(), "tenant", nil)
	require.ErrorIs(t, err, failure)
	_, err = client.ReadSkill(t.Context(), "tenant", nil, "notes", "")
	require.ErrorIs(t, err, failure)
	_, err = NewClient(store).ReadSkill(t.Context(), "tenant", nil, "review", "")
	require.ErrorIs(t, err, failure)
}
