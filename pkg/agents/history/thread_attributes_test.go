package history

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestThreadAttributesSurviveContinuationsAndFileReplay(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := NewFileConversationPersistence(dir)
	require.NoError(t, err)

	require.NoError(t, store.SaveMessages(ctx, "tenant", "parent", "parent", true, "r1", "", "child", "conv", nil, nil))
	// A continuation supplies no attributes; the thread keeps its own.
	require.NoError(t, store.SaveMessages(ctx, "tenant", "", "", false, "r2", "r1", "child", "conv", nil, nil))
	// A new thread in the same conversation keeps the attributes it was given.
	require.NoError(t, store.SaveMessages(ctx, "tenant", "", "", false, "r3", "", "sibling", "conv", nil, nil))
	require.NoError(t, store.Close())

	reopened, err := NewFileConversationPersistence(dir)
	require.NoError(t, err)
	defer reopened.Close()
	threads, err := reopened.ListThreads(ctx, "tenant", "parent")
	require.NoError(t, err)
	byID := map[string]ThreadInfo{}
	for _, thread := range threads {
		byID[thread.ThreadID] = thread
	}
	require.Equal(t, "parent", byID["child"].ParentThreadID)
	require.True(t, byID["child"].Hidden)
	require.Empty(t, byID["sibling"].ParentThreadID)
	require.False(t, byID["sibling"].Hidden)

	rows, err := reopened.LoadMessages(ctx, "tenant", "child", "")
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for _, row := range rows {
		require.Equal(t, "parent", row.ParentThreadID)
		require.True(t, row.Hidden)
	}
}
