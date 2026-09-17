package skills

import (
	"context"
	"fmt"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/stretchr/testify/require"
)

func TestGlobalSkillSetListsAndResolvesWithTenantPrecedence(t *testing.T) {
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
			set, err := NewSkillSet("library", store, WithGlobalNamespace("global"), WithPolicies(map[string]agents.SkillPolicy{"shared": agents.SkillRequired}))
			require.NoError(t, err)
			listed, err := set.ListSkills(ctx, "tenant", nil)
			require.NoError(t, err)
			require.Len(t, listed, 2)
			require.Equal(t, "review", listed[0].Name)
			require.NotContains(t, listed[0].Resources, "global-only.md")
			require.Equal(t, "shared", listed[1].Name)
			require.Equal(t, agents.SkillRequired, listed[1].Policy)
			content, err := set.ResolveSkill(ctx, "tenant", nil, "review", "")
			require.NoError(t, err)
			require.Contains(t, content, "tenant instructions")
			content, err = set.ResolveSkill(ctx, "tenant", nil, "shared", "")
			require.NoError(t, err)
			require.Contains(t, content, "shared instructions")
			content, err = set.ResolveSkill(ctx, "tenant", nil, "shared", "global-only.md")
			require.NoError(t, err)
			require.Equal(t, "global resource", content)
			_, err = set.ResolveSkill(ctx, "tenant", nil, "review", "global-only.md")
			require.ErrorIs(t, err, ErrNotFound, "resources must not mix namespaces")
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
				want = append(want, "global:", "global:next")
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
		if failNS == "tenant" {
			require.Equal(t, []string{"tenant"}, calls)
		} else {
			require.Equal(t, []string{"tenant", "global"}, calls)
		}
	}
}
