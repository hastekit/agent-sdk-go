package history

import (
	"context"
	"testing"

	"github.com/hastekit/hastekit-sdk-go/pkg/agents/messages"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func saveTurn(t *testing.T, p ConversationPersistenceAdapter, namespace, runID, prevID, threadID string) {
	t.Helper()
	err := p.SaveMessages(context.Background(), namespace, runID, prevID, threadID, "", []Message{messages.New("user", nil)}, nil)
	require.NoError(t, err)
}

func TestInMemoryListThreads(t *testing.T) {
	p := NewInMemoryConversationPersistence()

	saveTurn(t, p, "default", "m1", "", "thread-a")
	saveTurn(t, p, "default", "m2", "m1", "thread-a")
	saveTurn(t, p, "default", "m3", "", "thread-b")
	saveTurn(t, p, "other", "m4", "", "thread-c")

	threads, err := p.ListThreads(context.Background(), "default")
	require.NoError(t, err)
	require.Len(t, threads, 2)

	byID := map[string]ThreadInfo{}
	for _, th := range threads {
		byID[th.ThreadID] = th
	}
	assert.Equal(t, 2, byID["thread-a"].MessageCount)
	assert.Equal(t, 1, byID["thread-b"].MessageCount)
	assert.Equal(t, "m2", byID["thread-a"].LastRunID)

	// Empty namespace lists across all namespaces.
	all, err := p.ListThreads(context.Background(), "")
	require.NoError(t, err)
	assert.Len(t, all, 3)
}

func TestFileListThreadsSurvivesRestart(t *testing.T) {
	dir := t.TempDir()

	p1, err := NewFileConversationPersistence(dir)
	require.NoError(t, err)
	saveTurn(t, p1, "default", "m1", "", "thread-a")
	saveTurn(t, p1, "default", "m2", "m1", "thread-a")
	require.NoError(t, p1.Close())

	p2, err := NewFileConversationPersistence(dir)
	require.NoError(t, err)
	defer p2.Close()

	threads, err := p2.ListThreads(context.Background(), "default")
	require.NoError(t, err)
	require.Len(t, threads, 1)
	assert.Equal(t, "thread-a", threads[0].ThreadID)
	assert.Equal(t, 2, threads[0].MessageCount)
}
