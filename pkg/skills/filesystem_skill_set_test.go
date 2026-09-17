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

func TestFilesystemSkillsDiscoverChangesAndDefaultEnabled(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	write := func(name string) {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, name), 0755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, name, agents.SkillFileName), []byte("---\ndescription: Test instructions\n---\nRead guide.txt"), 0644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, name, "guide.txt"), []byte("reference"), 0644))
	}
	write("first")
	set, err := NewFilesystemSkillSet("local", dir)
	require.NoError(t, err)
	agent := agents.NewAgent(&agents.AgentOptions{Name: "files", Skills: []agents.SkillSet{set}})
	skills, err := set.ListSkills(ctx, "default", nil)
	require.NoError(t, err)
	require.Len(t, skills, 1)
	require.Equal(t, "first", skills[0].Name)
	require.Equal(t, agents.SkillEnabled, skills[0].Policy)
	result, err := set.ResolveSkill(ctx, "default", nil, "first", "guide.txt")
	require.NoError(t, err)
	require.Equal(t, "reference", result)
	write("second")
	catalog, err := agent.ListSkills(ctx, "default", nil, agents.SkillSelection{})
	require.NoError(t, err)
	require.Len(t, catalog, 2)
	require.True(t, catalog[0].Enabled)
	require.True(t, catalog[1].Enabled)
	catalog, err = agent.ListSkills(ctx, "default", nil, agents.SkillSelection{Disable: []string{"first"}})
	require.NoError(t, err)
	require.False(t, catalog[0].Enabled)
	require.NoError(t, os.RemoveAll(filepath.Join(dir, "first")))
	catalog, err = agent.ListSkills(ctx, "default", nil, agents.SkillSelection{})
	require.NoError(t, err)
	require.Len(t, catalog, 1)
	require.Equal(t, "second", catalog[0].Name)
}

func TestFSSkillsReadResourcesConcurrently(t *testing.T) {
	set, err := NewFSSkillSet("embedded", fstest.MapFS{
		"skills/review/SKILL.md": {Data: []byte("---\ndescription: Review work\n---\nInstructions")},
		"skills/review/ref.md":   {Data: []byte("Details")},
	})
	require.NoError(t, err)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			skills, err := set.ListSkills(context.Background(), "default", nil)
			require.NoError(t, err)
			require.Len(t, skills, 1)
			require.Equal(t, agents.SkillEnabled, skills[0].Policy)
			content, err := set.ResolveSkill(context.Background(), "default", nil, "review", "ref.md")
			require.NoError(t, err)
			require.Equal(t, "Details", content)
		}()
	}
	wg.Wait()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = set.ListSkills(ctx, "default", nil)
	require.ErrorIs(t, err, context.Canceled)
	_, err = set.ResolveSkill(ctx, "default", nil, "review", "")
	require.ErrorIs(t, err, context.Canceled)
}

func TestFilesystemSkillsRejectInvalidConfiguration(t *testing.T) {
	_, err := NewFilesystemSkillSet("bad/name", t.TempDir())
	require.Error(t, err)
	_, err = NewFilesystemSkillSet("local", filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
	_, err = NewFSSkillSet("local", nil)
	require.Error(t, err)
	_, err = NewFSSkillSet("local", fstest.MapFS{"bad/SKILL.md": {Data: []byte("no description")}})
	require.Error(t, err)
}
