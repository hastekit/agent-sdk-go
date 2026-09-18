package skills

import (
	"context"
	"fmt"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/stretchr/testify/require"
)

func TestGlobalSkillSetListsAndResolvesWithGlobalPrecedence(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			for ns, bundles := range map[string][]Bundle{
				"tenant": {bundle("review", "tenant instructions")},
				"global": {bundle("review", "global instructions"), bundle("shared", "shared instructions")},
			} {
				for _, b := range bundles {
					if ns == "global" {
						b.Files["global-only.md"] = []byte("global resource")
					}
					_, err := store.Put(ctx, ns, b)
					require.NoError(t, err)
				}
			}
			set, err := NewSkillSet("library", store, WithGlobalNamespace("global"), WithRequiredSkills("shared"))
			require.NoError(t, err)
			listed, err := set.ListSkills(ctx, "tenant", nil)
			require.NoError(t, err)
			require.Len(t, listed, 2)
			byName := map[string]agents.Skill{}
			for _, skill := range listed {
				byName[skill.Name] = skill
				require.True(t, skill.Global)
			}
			require.Equal(t, "review", byName["review"].Name)
			require.Contains(t, byName["review"].Resources, "global-only.md")
			require.Equal(t, "shared", byName["shared"].Name)
			require.True(t, byName["shared"].Required)
			content, err := set.ResolveSkill(ctx, "tenant", nil, "review", "")
			require.NoError(t, err)
			require.Contains(t, content, "global instructions")
			content, err = set.ResolveSkill(ctx, "tenant", nil, "shared", "")
			require.NoError(t, err)
			require.Contains(t, content, "shared instructions")
			content, err = set.ResolveSkill(ctx, "tenant", nil, "shared", "global-only.md")
			require.NoError(t, err)
			require.Equal(t, "global resource", content)
			content, err = set.ResolveSkill(ctx, "tenant", nil, "review", "global-only.md")
			require.NoError(t, err)
			require.Equal(t, "global resource", content)
			_, err = set.ResolveSkill(ctx, "tenant", nil, "missing", "")
			require.ErrorIs(t, err, ErrNotFound)
			listed, err = set.ListSkills(ctx, "", nil)
			require.NoError(t, err)
			require.Len(t, listed, 2, "empty caller namespace still sees global skills")
		})
	}
}

type globalSkillStore struct {
	Store
	list func(context.Context, string, ListOptions) (Page, error)
	get  func(context.Context, string, string) (Bundle, error)
}

func (s globalSkillStore) List(ctx context.Context, ns string, opts ListOptions) (Page, error) {
	return s.list(ctx, ns, opts)
}
func (s globalSkillStore) Get(ctx context.Context, ns, name string) (Bundle, error) {
	return s.get(ctx, ns, name)
}

func TestGlobalSkillSetPaginationAndMatchingNamespaces(t *testing.T) {
	for _, global := range []string{"", "tenant", "global"} {
		t.Run("global="+global, func(t *testing.T) {
			var calls []string
			store := globalSkillStore{list: func(_ context.Context, ns string, opts ListOptions) (Page, error) {
				calls = append(calls, ns+":"+opts.Cursor)
				if opts.Cursor == "" {
					return Page{Skills: []Metadata{{Name: ns + "-one"}}, NextCursor: "next"}, nil
				}
				require.Equal(t, "next", opts.Cursor)
				return Page{Skills: []Metadata{{Name: ns + "-two"}}}, nil
			}}
			set, err := NewSkillSet("library", store, WithGlobalNamespace(global))
			require.NoError(t, err)
			listed, err := set.ListSkills(t.Context(), "tenant", nil)
			require.NoError(t, err)
			want := []string{"tenant:", "tenant:next"}
			if global == "global" {
				want = append([]string{"global:", "global:next"}, want...)
			}
			require.Equal(t, want, calls)
			require.Len(t, listed, len(want))
		})
	}
}

func TestGlobalSkillSetDoesNotHideStoreErrors(t *testing.T) {
	failure := fmt.Errorf("storage unavailable")
	for _, failNS := range []string{"tenant", "global"} {
		var calls []string
		store := globalSkillStore{
			list: func(_ context.Context, ns string, _ ListOptions) (Page, error) {
				if ns == failNS {
					return Page{}, failure
				}
				return Page{}, nil
			},
			get: func(_ context.Context, ns, _ string) (Bundle, error) {
				calls = append(calls, ns)
				if ns == failNS {
					return Bundle{}, failure
				}
				return Bundle{}, fmt.Errorf("missing: %w", ErrNotFound)
			},
		}
		set, err := NewSkillSet("library", store, WithGlobalNamespace("global"))
		require.NoError(t, err)
		_, err = set.ListSkills(t.Context(), "tenant", nil)
		require.ErrorIs(t, err, failure)
		_, err = set.ResolveSkill(t.Context(), "tenant", nil, "review", "")
		require.ErrorIs(t, err, failure)
		if failNS == "global" {
			require.Equal(t, []string{"global"}, calls)
		} else {
			require.Equal(t, []string{"global", "tenant"}, calls)
		}
	}
}

func TestGlobalRequiredSkillCannotBeReplacedOrDisabled(t *testing.T) {
	for backend, store := range stores(t) {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			global := bundle("review", "developer instructions")
			local := bundle("review", "user instructions")
			local.Files["user-only.md"] = []byte("user resource")
			_, err := store.Put(ctx, "global", global)
			require.NoError(t, err)
			_, err = store.Put(ctx, "user", local)
			require.NoError(t, err)
			set, err := NewSkillSet("library", store, WithGlobalNamespace("global"), WithRequiredSkills("review"))
			require.NoError(t, err)
			agent := agents.NewAgent(&agents.AgentOptions{Name: "test", Skills: []agents.SkillSet{set}})
			listed, err := agent.ListSkills(ctx, "user", nil, agents.SkillSelection{Disable: []string{"review"}})
			require.NoError(t, err)
			require.Len(t, listed, 1)
			require.True(t, listed[0].Global)
			require.True(t, listed[0].Required)
			require.True(t, listed[0].Enabled)
			content, err := set.ResolveSkill(ctx, "user", nil, "review", "")
			require.NoError(t, err)
			require.Equal(t, "developer instructions", content)
			_, err = set.ResolveSkill(ctx, "user", nil, "review", "user-only.md")
			require.ErrorIs(t, err, ErrNotFound)
			// When the developer removes the global skill, the user's copy is optional.
			require.NoError(t, store.Delete(ctx, "global", "review"))
			listed, err = agent.ListSkills(ctx, "user", nil, agents.SkillSelection{Disable: []string{"review"}})
			require.NoError(t, err)
			require.Len(t, listed, 1)
			require.False(t, listed[0].Global)
			require.False(t, listed[0].Required)
			require.False(t, listed[0].Enabled)
		})
	}
}
