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

func TestDirSourceDiscoversChangesAsGlobals(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	write := func(name string) {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, name), 0755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, name, agents.SkillFileName), []byte("---\ndescription: Test instructions\n---\nRead guide.txt"), 0644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, name, "guide.txt"), []byte("reference"), 0644))
	}
	write("first")
	source, err := NewDirSource(dir)
	require.NoError(t, err)
	skills, err := source.List(ctx)
	require.NoError(t, err)
	require.Len(t, skills, 1)
	require.Equal(t, "first", skills[0].Name)
	result, err := source.Read(ctx, "first", "guide.txt")
	require.NoError(t, err)
	require.Equal(t, "reference", result)

	// Folder skills are globals: always enabled, and changes appear on the next listing.
	agent := agents.NewAgent(&agents.AgentOptions{Name: "files", SkillClient: NewClient(nil).WithGlobalSkills(source)})
	write("second")
	catalog, err := agent.ListSkills(ctx, "tenant", nil, agents.SkillSelection{Disable: []string{"first"}})
	require.NoError(t, err)
	require.Len(t, catalog, 2)
	for _, listed := range catalog {
		require.True(t, listed.Global)
		require.True(t, listed.Enabled)
	}
	require.NoError(t, os.RemoveAll(filepath.Join(dir, "first")))
	catalog, err = agent.ListSkills(ctx, "tenant", nil, agents.SkillSelection{})
	require.NoError(t, err)
	require.Len(t, catalog, 1)
	require.Equal(t, "second", catalog[0].Name)
}

func TestFSSourceReadsResourcesConcurrently(t *testing.T) {
	source, err := NewFSSource(fstest.MapFS{
		"skills/review/SKILL.md": {Data: []byte("---\ndescription: Review work\n---\nInstructions")},
		"skills/review/ref.md":   {Data: []byte("Details")},
	})
	require.NoError(t, err)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			skills, err := source.List(context.Background())
			require.NoError(t, err)
			require.Len(t, skills, 1)
			content, err := source.Read(context.Background(), "review", "ref.md")
			require.NoError(t, err)
			require.Equal(t, "Details", content)
		})
	}
	wg.Wait()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = source.List(ctx)
	require.ErrorIs(t, err, context.Canceled)
	_, err = source.Read(ctx, "review", "")
	require.ErrorIs(t, err, context.Canceled)
}

func TestSourcesRejectInvalidConfiguration(t *testing.T) {
	_, err := NewDirSource(filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
	_, err = NewFSSource(nil)
	require.Error(t, err)
	_, err = NewFSSource(fstest.MapFS{"bad/SKILL.md": {Data: []byte("no description")}})
	require.Error(t, err)
	_, err = NewBundleSource(Bundle{Files: map[string][]byte{"SKILL.md": []byte("no frontmatter")}})
	require.ErrorIs(t, err, ErrInvalid)
}

func TestBundleSourceCopiesItsInput(t *testing.T) {
	b := bundle("review", "original")
	source, err := NewBundleSource(b)
	require.NoError(t, err)
	b.Files["refs/check.md"][0] = 'X'
	b.Files["late.md"] = []byte("added after construction")
	content, err := source.Read(t.Context(), "review", "refs/check.md")
	require.NoError(t, err)
	require.Equal(t, "check", content)
	_, err = source.Read(t.Context(), "review", "late.md")
	require.ErrorIs(t, err, ErrNotFound)
}
