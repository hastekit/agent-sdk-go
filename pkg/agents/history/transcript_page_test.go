package history

import (
	"context"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestTranscriptPages(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"memory", "file", "legacy"} {
		t.Run(kind, func(t *testing.T) {
			var p ConversationPersistenceAdapter = NewInMemoryConversationPersistence()
			if kind == "file" {
				file, err := NewFileConversationPersistence(t.TempDir())
				require.NoError(t, err)
				defer file.Close()
				p = file
			}
			if kind == "legacy" {
				p = struct{ ConversationPersistenceAdapter }{p}
			}
			// Intentionally not lexical order: IDs are opaque.
			ids := []string{"z", "a", "m", "b", "x"}
			previous := ""
			for _, id := range ids {
				require.NoError(t, p.SaveMessages(ctx, "ns", "default", id, previous, "thread", "conversation", []Message{userBundle("user", id)}, nil))
				previous = id
			}
			page, err := LoadTranscriptPage(ctx, p, "ns", "thread", TranscriptPageOptions{Limit: 2})
			require.NoError(t, err)
			require.Equal(t, "b", page.Rows[0].RunID)
			require.Equal(t, "x", page.Rows[1].RunID)
			require.Equal(t, "b", page.NextBeforeRunID)
			require.Equal(t, "x", page.Latest.RunID)
			// A concurrent append doesn't shift the older-page boundary.
			require.NoError(t, p.SaveMessages(ctx, "ns", "default", "new", "x", "thread", "conversation", nil, nil))
			page, err = LoadTranscriptPage(ctx, p, "ns", "thread", TranscriptPageOptions{Limit: 2, BeforeRunID: page.NextBeforeRunID})
			require.NoError(t, err)
			require.Equal(t, []string{"a", "m"}, []string{page.Rows[0].RunID, page.Rows[1].RunID})
			require.Equal(t, "new", page.Latest.RunID)
			page, err = LoadTranscriptPage(ctx, p, "ns", "thread", TranscriptPageOptions{Limit: 2, BeforeRunID: page.NextBeforeRunID})
			require.NoError(t, err)
			require.Len(t, page.Rows, 1)
			require.Equal(t, "z", page.Rows[0].RunID)
			require.Empty(t, page.NextBeforeRunID)
			_, err = LoadTranscriptPage(ctx, p, "other", "thread", TranscriptPageOptions{Limit: 2, BeforeRunID: "b"})
			require.ErrorIs(t, err, ErrInvalidTranscriptCursor)
			_, err = LoadTranscriptPage(ctx, p, "ns", "thread", TranscriptPageOptions{Limit: 2, BeforeRunID: "missing"})
			require.ErrorIs(t, err, ErrInvalidTranscriptCursor)
			empty, err := LoadTranscriptPage(ctx, p, "ns", "empty", TranscriptPageOptions{Limit: 2})
			require.NoError(t, err)
			require.Empty(t, empty.Rows)
			require.Nil(t, empty.Latest)
		})
	}
}
