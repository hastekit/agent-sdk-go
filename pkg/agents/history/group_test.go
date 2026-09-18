package history

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConversationGroups(t *testing.T) {
	for _, backend := range []string{"memory", "file"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			var p ConversationPersistenceAdapter
			dir := t.TempDir()
			if backend == "memory" {
				p = NewInMemoryConversationPersistence()
			} else {
				f, err := NewFileConversationPersistence(dir)
				require.NoError(t, err)
				p = f
			}
			save := func(ns, run, prev, thread, conversation, group string) {
				t.Helper()
				require.NoError(t, p.SaveMessages(ctx, ns, group, run, prev, thread, conversation, nil, nil))
			}
			save("tenant", "normal", "", "normal", "normal-conv", "")
			save("tenant", "project", "", "project", "project-conv", "project-1")
			save("tenant", "routine", "", "routine", "routine-conv", "routine-1")
			save("tenant", "followup", "routine", "routine", "routine-conv", "")
			save("tenant", "fork", "routine", "routine", "routine-conv", "")
			save("tenant", "related", "", "related", "routine-conv", "")
			save("other", "other", "", "other", "other-conv", "routine-1")
			// A metadata-only routine marker must not classify a normal conversation.
			require.NoError(t, p.SaveMessages(ctx, "tenant", "default", "metadata", "", "metadata", "", nil, map[string]any{
				RunContextMetaKey: map[string]any{RoutineIDContextKey: "routine-1"},
			}))
			if f, ok := p.(*FileConversationPersistence); ok {
				require.NoError(t, f.Close())
				reopened, err := NewFileConversationPersistence(dir)
				require.NoError(t, err)
				defer reopened.Close()
				p = reopened
			}
			lister := p.(ThreadLister)
			normal, err := lister.ListThreads(ctx, "tenant", "default")
			require.NoError(t, err)
			require.Len(t, normal, 2)
			for _, thread := range normal {
				require.Equal(t, DefaultGroupID, thread.GroupID)
			}
			empty, err := lister.ListThreads(ctx, "tenant", "")
			require.NoError(t, err)
			require.Equal(t, normal, empty)
			projects, err := lister.ListThreads(ctx, "tenant", "project-1")
			require.NoError(t, err)
			require.Len(t, projects, 1)
			routines, err := lister.ListThreads(ctx, "tenant", "routine-1")
			require.NoError(t, err)
			require.Len(t, routines, 3)
			for _, thread := range routines {
				require.Equal(t, "routine-1", thread.GroupID)
				require.Equal(t, "tenant", thread.Namespace)
			}
			missing, err := lister.ListThreads(ctx, "tenant", "unknown")
			require.NoError(t, err)
			require.Empty(t, missing)
			transcript, err := p.(TranscriptReader).LoadTranscript(ctx, "tenant", "routine")
			require.NoError(t, err)
			for _, row := range transcript {
				require.Equal(t, "routine-1", row.GroupID)
			}
		})
	}
}

func TestRunManagerGroupInheritance(t *testing.T) {
	ctx := context.Background()
	p := NewInMemoryConversationPersistence()
	manager := NewConversationManager(p)
	run, err := NewRun(ctx, manager, "tenant", "thread", "", WithGroupID("project-1"), WithDefaultConversationID("conversation"))
	require.NoError(t, err)
	require.NoError(t, run.SaveMessages(ctx))
	restored, err := NewRun(ctx, manager, "tenant", "thread", "", WithGroupID("different-project"))
	require.NoError(t, err)
	require.Equal(t, "project-1", restored.groupID)
	require.NoError(t, restored.SaveMessages(ctx))
	threads, err := p.ListThreads(ctx, "tenant", "project-1")
	require.NoError(t, err)
	require.Len(t, threads, 1)
	normal, err := p.ListThreads(ctx, "tenant", "default")
	require.NoError(t, err)
	require.Empty(t, normal)
}

func TestNewRunDefaultsGroup(t *testing.T) {
	p := NewInMemoryConversationPersistence()
	ctx := context.Background()
	for _, option := range []RunOption{func(*ConversationRunManager) {}, WithGroupID("")} {
		run, err := NewRun(ctx, NewConversationManager(p), "tenant", p.NewRunID(ctx), "", option)
		require.NoError(t, err)
		require.Equal(t, DefaultGroupID, run.groupID)
		require.NoError(t, run.SaveMessages(ctx))
	}
	threads, err := p.ListThreads(ctx, "tenant", DefaultGroupID)
	require.NoError(t, err)
	require.Len(t, threads, 2)
}

func TestNewConversationDoesNotInheritEmptyPreviousRun(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	p, err := NewFileConversationPersistence(dir)
	require.NoError(t, err)
	// Legacy conversation files can contain records without a run_id. Replaying
	// them indexes a row under "", which is not a parent of a new conversation.
	require.NoError(t, p.SaveMessages(ctx, "default", DefaultGroupID, "", "", "legacy-thread", "legacy-conversation", nil, nil))
	require.NoError(t, p.Close())
	p, err = NewFileConversationPersistence(dir)
	require.NoError(t, err)
	defer p.Close()
	require.NoError(t, p.SaveMessages(ctx, "default", "routine-id", "new-run", "", "new-thread", "new-conversation", nil, nil))
	threads, err := p.ListThreads(ctx, "default", "routine-id")
	require.NoError(t, err)
	require.Len(t, threads, 1, "an empty previousRunID must not inherit from a legacy row with an empty run ID")
	require.Equal(t, "new-thread", threads[0].ThreadID)
}
