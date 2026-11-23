package skills

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/stretchr/testify/require"
)

func TestDirStoreDiscoversChangesAsGlobals(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	write := func(name string) {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, name), 0755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, name, agents.SkillFileName), []byte("---\ndescription: Test instructions\n---\nRead guide.txt"), 0644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, name, "guide.txt"), []byte("reference"), 0644))
	}
	write("first")
	store, err := NewDirStore(dir)
	require.NoError(t, err)
	page, err := store.List(ctx, "", ListOptions{})
	require.NoError(t, err)
	require.Equal(t, []Metadata{{Name: "first", Description: "Test instructions", Resources: []string{"guide.txt"}, FileLocation: filepath.Join(dir, "first", agents.SkillFileName)}}, page.Skills)
	bundle, err := store.Get(ctx, "", "first")
	require.NoError(t, err)
	require.Equal(t, "reference", string(bundle.Files["guide.txt"]))

	// Folder skills are globals only: no other namespace lists or reads them.
	page, err = store.List(ctx, "tenant", ListOptions{})
	require.NoError(t, err)
	require.Empty(t, page.Skills)
	_, err = store.Get(ctx, "tenant", "first")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = store.Get(ctx, "", "missing")
	require.ErrorIs(t, err, ErrNotFound)

	// Globals are always enabled, and changes appear on the next listing.
	agent := agents.NewAgent(&agents.AgentOptions{Name: "files", SkillClient: NewClient(store)})
	write("second")
	listed, err := agent.ListSkills(ctx, "tenant", nil, agents.SkillSelection{Disable: []string{"first"}})
	require.NoError(t, err)
	require.Len(t, listed, 2)
	for _, skill := range listed {
		require.True(t, skill.Global)
		require.True(t, skill.Enabled)
	}
	require.NoError(t, os.RemoveAll(filepath.Join(dir, "first")))
	listed, err = agent.ListSkills(ctx, "tenant", nil, agents.SkillSelection{})
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, "second", listed[0].Name)
}

func TestFSStoreReadsConcurrently(t *testing.T) {
	store, err := NewFSStore(fstest.MapFS{
		"skills/review/SKILL.md": {Data: []byte("---\ndescription: Review work\n---\nInstructions")},
		"skills/review/ref.md":   {Data: []byte("Details")},
	})
	require.NoError(t, err)
	client := NewClient(store)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			skills, err := client.ListSkills(context.Background(), "tenant", nil)
			require.NoError(t, err)
			require.Len(t, skills, 1)
			content, err := client.ReadSkill(context.Background(), "tenant", nil, "review", "ref.md")
			require.NoError(t, err)
			require.Equal(t, "Details", content)
		})
	}
	wg.Wait()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = store.List(ctx, "", ListOptions{})
	require.ErrorIs(t, err, context.Canceled)
	_, err = store.Get(ctx, "", "review")
	require.ErrorIs(t, err, context.Canceled)
}

func TestBuiltinStoresRejectInvalidConfiguration(t *testing.T) {
	_, err := NewDirStore(filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
	_, err = NewFSStore(nil)
	require.Error(t, err)
	_, err = NewFSStore(fstest.MapFS{"bad/SKILL.md": {Data: []byte("no description")}})
	require.Error(t, err)
	_, err = NewInlineStore(Bundle{Files: map[string][]byte{"SKILL.md": []byte("no frontmatter")}})
	require.ErrorIs(t, err, ErrInvalid)
}

func TestInlineStoreCopiesItsInputAndOutput(t *testing.T) {
	b := bundle("review", "original")
	store, err := NewInlineStore(b)
	require.NoError(t, err)
	b.Files["refs/check.md"][0] = 'X'
	b.Files["late.md"] = []byte("added after construction")
	got, err := store.Get(t.Context(), "", "review")
	require.NoError(t, err)
	require.Equal(t, "check", string(got.Files["refs/check.md"]))
	require.NotContains(t, got.Files, "late.md")
	got.Files["refs/check.md"][0] = 'Y'
	again, err := store.Get(t.Context(), "", "review")
	require.NoError(t, err)
	require.Equal(t, "check", string(again.Files["refs/check.md"]))
}

func TestInMemoryStoresPageByName(t *testing.T) {
	store := testInlineStore(t, bundle("c", "c"), bundle("a", "a"), bundle("b", "b"))
	var names []string
	cursor := ""
	for {
		page, err := store.List(t.Context(), "", ListOptions{Limit: 2, Cursor: cursor})
		require.NoError(t, err)
		require.LessOrEqual(t, len(page.Skills), 2)
		for _, m := range page.Skills {
			names = append(names, m.Name)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	require.Equal(t, []string{"a", "b", "c"}, names)
}
