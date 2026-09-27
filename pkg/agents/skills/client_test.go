package skills

import (
	"context"
	"fmt"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/stretchr/testify/require"
)

func inlineSource(t *testing.T, bundles ...Bundle) Source {
	t.Helper()
	source, err := NewBundleSource(bundles...)
	require.NoError(t, err)
	return source
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
			client := NewClient(store).WithGlobalSkills(inlineSource(t, global))

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
			catalog, err = agents.NewAgent(&agents.AgentOptions{Name: "plain", SkillClient: NewClient(store)}).ListSkills(ctx, "tenant", nil, agents.SkillSelection{Disable: []string{"review"}})
			require.NoError(t, err)
			require.Len(t, catalog, 2)
			require.Equal(t, "notes", catalog[0].Name)
			require.Equal(t, "review", catalog[1].Name)
			require.False(t, catalog[1].Global)
			require.False(t, catalog[1].Enabled)
		})
	}
}

func TestClientGivesEachAgentItsOwnGlobals(t *testing.T) {
	ctx := t.Context()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	base := NewClient(store)
	reviewer := base.WithGlobalSkills(inlineSource(t, bundle("review", "review")))
	writer := base.WithGlobalSkills(inlineSource(t, bundle("style", "style")))
	both := reviewer.WithGlobalSkills(inlineSource(t, bundle("style", "style")))
	names := func(client *Client) []string {
		t.Helper()
		listed, err := client.ListSkills(ctx, "tenant", nil)
		require.NoError(t, err)
		var result []string
		for _, skill := range listed {
			result = append(result, skill.Name)
		}
		return result
	}

	// Every view shares the user store; globals stay per view.
	_, err = store.Put(ctx, "tenant", bundle("mine", "user"))
	require.NoError(t, err)
	require.Equal(t, []string{"mine"}, names(base))
	require.Equal(t, []string{"review", "mine"}, names(reviewer))
	require.Equal(t, []string{"style", "mine"}, names(writer))
	require.Equal(t, []string{"review", "style", "mine"}, names(both))
}

func TestClientRejectsAmbiguousGlobals(t *testing.T) {
	client := NewClient(nil).WithGlobalSkills(inlineSource(t, bundle("review", "a")), inlineSource(t, bundle("review", "b")))
	_, err := client.ListSkills(t.Context(), "tenant", nil)
	require.ErrorContains(t, err, "duplicate global skill")
	_, err = client.ReadSkill(t.Context(), "tenant", nil, "review", "")
	require.ErrorContains(t, err, "duplicate global skill")
	_, err = NewClient(nil).WithGlobalSkills(nil).ListSkills(t.Context(), "tenant", nil)
	require.Error(t, err)
	_, err = NewBundleSource(bundle("review", "a"), bundle("review", "b"))
	require.ErrorIs(t, err, ErrInvalid)
}

type pagedStore struct {
	Store
	list func(context.Context, string, ListOptions) (Page, error)
	get  func(context.Context, string, string) (Bundle, error)
}

func (s pagedStore) List(ctx context.Context, ns string, opts ListOptions) (Page, error) {
	return s.list(ctx, ns, opts)
}
func (s pagedStore) Get(ctx context.Context, ns, name string) (Bundle, error) {
	return s.get(ctx, ns, name)
}

func TestClientPaginatesUserSkills(t *testing.T) {
	var calls []string
	store := pagedStore{list: func(_ context.Context, ns string, opts ListOptions) (Page, error) {
		calls = append(calls, ns+":"+opts.Cursor)
		if opts.Cursor == "" {
			return Page{Skills: []Metadata{{Name: "one"}}, NextCursor: "next"}, nil
		}
		return Page{Skills: []Metadata{{Name: "two"}}}, nil
	}}
	listed, err := NewClient(store).ListSkills(t.Context(), "tenant", nil)
	require.NoError(t, err)
	require.Equal(t, []string{"tenant:", "tenant:next"}, calls)
	require.Len(t, listed, 2)

	looping := pagedStore{list: func(context.Context, string, ListOptions) (Page, error) {
		return Page{NextCursor: "same"}, nil
	}}
	_, err = NewClient(looping).ListSkills(t.Context(), "tenant", nil)
	require.ErrorContains(t, err, "repeated pagination cursor")
}

func TestClientDoesNotHideStoreErrors(t *testing.T) {
	failure := fmt.Errorf("storage unavailable")
	store := pagedStore{
		list: func(context.Context, string, ListOptions) (Page, error) { return Page{}, failure },
		get:  func(context.Context, string, string) (Bundle, error) { return Bundle{}, failure },
	}
	client := NewClient(store)
	_, err := client.ListSkills(t.Context(), "tenant", nil)
	require.ErrorIs(t, err, failure)
	_, err = client.ReadSkill(t.Context(), "tenant", nil, "review", "")
	require.ErrorIs(t, err, failure)

	// A failing global source fails listing rather than silently dropping developer skills.
	source, err := NewStoreSource(store, "global")
	require.NoError(t, err)
	_, err = NewClient(nil).WithGlobalSkills(source).ListSkills(t.Context(), "tenant", nil)
	require.ErrorIs(t, err, failure)
}

func TestStoreSourceServesANamespaceAsGlobals(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			_, err := store.Put(ctx, "library", bundle("shared", "shared instructions"))
			require.NoError(t, err)
			source, err := NewStoreSource(store, "library")
			require.NoError(t, err)
			client := NewClient(store).WithGlobalSkills(source)
			listed, err := client.ListSkills(ctx, "tenant", nil)
			require.NoError(t, err)
			require.Len(t, listed, 1)
			require.True(t, listed[0].Global)
			content, err := client.ReadSkill(ctx, "tenant", nil, "shared", "refs/check.md")
			require.NoError(t, err)
			require.Equal(t, "check", content)

			// Later saves to the library namespace are visible on the next listing.
			_, err = store.Put(ctx, "library", bundle("added", "added later"))
			require.NoError(t, err)
			listed, err = client.ListSkills(ctx, "tenant", nil)
			require.NoError(t, err)
			require.Len(t, listed, 2)
		})
	}
	_, err := NewStoreSource(nil, "library")
	require.ErrorIs(t, err, ErrInvalid)
	_, err = NewStoreSource(stores(t)["filesystem"], "")
	require.ErrorIs(t, err, ErrInvalid)
}
