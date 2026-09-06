package history

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPersistenceNamespaceIsolation(t *testing.T) {
	for _, mode := range []string{"memory", "file"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			var p ConversationPersistenceAdapter = NewInMemoryConversationPersistence()
			dir := t.TempDir()
			if mode == "file" {
				f, err := NewFileConversationPersistence(dir)
				require.NoError(t, err)
				p = f
			}
			for _, ns := range []string{"tenant-a", "tenant-b", ""} {
				require.NoError(t, p.SaveMessages(ctx, ns, "run1", "", "thread", "conv", []Message{{ID: ns + "-first"}}, nil))
				require.NoError(t, p.SaveMessages(ctx, ns, "run2", "run1", "thread", "conv", []Message{{ID: ns + "-second"}}, nil))
				require.NoError(t, p.SaveSummary(ctx, ns, Summary{ID: "summary", ThreadID: "thread", LastSummarizedRunID: "run1", SummaryMessage: Message{ID: ns + "-summary"}}))
			}
			check := func() {
				for _, ns := range []string{"tenant-a", "tenant-b", ""} {
					got, err := p.LoadMessages(ctx, ns, "thread", "run2")
					require.NoError(t, err)
					require.Len(t, got, 2)
					require.Equal(t, ns+"-summary", got[0].Messages[0].ID)
					require.Equal(t, ns+"-second", got[1].Messages[0].ID)
					transcript, err := LoadTranscript(ctx, p, ns, "thread")
					require.NoError(t, err)
					require.Len(t, transcript, 2)
					require.Equal(t, ns+"-first", transcript[0].Messages[0].ID)
					threads, err := p.(ThreadLister).ListThreads(ctx, ns)
					require.NoError(t, err)
					if ns != "" {
						require.Len(t, threads, 1)
						require.Equal(t, ns, threads[0].Namespace)
					}
				}
				got, err := p.LoadMessages(ctx, "stranger", "thread", "run2")
				require.NoError(t, err)
				require.Empty(t, got)
				transcript, err := LoadTranscript(ctx, p, "stranger", "thread")
				require.NoError(t, err)
				require.Empty(t, transcript)
			}
			check()
			if mode == "file" {
				require.NoError(t, p.(*FileConversationPersistence).Close())
				reopened, err := NewFileConversationPersistence(dir)
				require.NoError(t, err)
				defer reopened.Close()
				p = reopened
				check()
			}
		})
	}
}

func TestNamespaceCannotContinueAnotherNamespacesRun(t *testing.T) {
	ctx := context.Background()
	p := NewInMemoryConversationPersistence()
	require.NoError(t, p.SaveMessages(ctx, "a", "secret-run", "", "secret-thread", "secret-conv", []Message{{ID: "secret"}}, nil))
	require.NoError(t, p.SaveMessages(ctx, "b", "new-run", "secret-run", "secret-thread", "new-conv", []Message{{ID: "public"}}, nil))
	saved := p.getMessage("b", "new-run")
	require.Equal(t, "new-conv", saved.ConversationID)
	got, err := p.LoadMessages(ctx, "b", saved.ThreadID, "new-run")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "public", got[0].Messages[0].ID)
}
