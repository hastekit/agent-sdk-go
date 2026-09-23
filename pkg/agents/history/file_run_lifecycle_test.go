package history

import (
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/stretchr/testify/require"
)

func TestFileRunLifecycleSurvivesReopen(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	p, err := NewFileConversationPersistence(dir)
	require.NoError(t, err)
	run, err := NewRun(ctx, NewConversationManager(p), "tenant", "thread", "", WithRunID("run"))
	require.NoError(t, err)
	run.AddMessages(ctx, userBundle("user", "Plan my trip"))
	require.NoError(t, run.SaveMessages(ctx))
	require.NoError(t, p.Close())

	p, err = NewFileConversationPersistence(dir)
	require.NoError(t, err)
	threads, err := p.ListThreads(ctx, "tenant", "default")
	require.NoError(t, err)
	require.Len(t, threads, 1)
	require.Equal(t, "Plan my trip", threads[0].Title)
	opened := threads[0]
	rows, err := p.LoadMessages(ctx, "tenant", "thread", "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NotContains(t, rows[0].Meta, agentstate.CompletedAtMetaKey)
	require.EqualValues(t, agentstate.RunStatusInProgress, rows[0].Meta["run_state"].(map[string]any)["status"])

	run, err = NewRun(ctx, NewConversationManager(p), "tenant", "thread", "")
	require.NoError(t, err)
	run.AddMessages(ctx, userBundle("agent", "Here is your itinerary"))
	require.NoError(t, run.SaveMessages(ctx))
	// No new messages: completion must still persist state and recency.
	run.RunState.TransitionToComplete()
	require.NoError(t, run.SaveMessages(ctx))
	threads, err = p.ListThreads(ctx, "tenant", "default")
	require.NoError(t, err)
	require.Equal(t, opened.CreatedAt, threads[0].CreatedAt)
	require.True(t, threads[0].UpdatedAt.After(opened.UpdatedAt))
	completed := threads[0]
	require.NoError(t, p.Close())

	p, err = NewFileConversationPersistence(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })
	threads, err = p.ListThreads(ctx, "tenant", "default")
	require.NoError(t, err)
	require.Equal(t, completed.Title, threads[0].Title)
	require.True(t, completed.UpdatedAt.Equal(threads[0].UpdatedAt))
	rows, err = p.LoadMessages(ctx, "tenant", "thread", "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Len(t, rows[0].Messages, 2)
	require.Equal(t, "run", rows[0].RunID)
	require.Contains(t, rows[0].Meta, agentstate.CompletedAtMetaKey)
	require.EqualValues(t, agentstate.RunStatusCompleted, rows[0].Meta["run_state"].(map[string]any)["status"])
}
