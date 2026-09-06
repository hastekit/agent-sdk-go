package history

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRecoveryRemainsAppendableAcrossRestarts(t *testing.T) {
	for _, suffix := range []string{"partial", "missing-newline"} {
		t.Run(suffix, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			p, err := NewFileConversationPersistence(dir)
			require.NoError(t, err)
			require.NoError(t, p.SaveMessages(ctx, "ns", "run1", "", "thread", "conv", []Message{{ID: "first"}}, nil))
			require.NoError(t, p.Close())
			path := filepath.Join(dir, "conv.jsonl")
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			if suffix == "partial" {
				data = append(data, []byte(`{"type":`)...)
			} else {
				data = data[:len(data)-1]
			}
			require.NoError(t, os.WriteFile(path, data, 0600))
			p, err = NewFileConversationPersistence(dir)
			require.NoError(t, err)
			require.NoError(t, p.SaveMessages(ctx, "ns", "run2", "run1", "thread", "conv", []Message{{ID: "second"}}, nil))
			require.NoError(t, p.Close())
			p, err = NewFileConversationPersistence(dir)
			require.NoError(t, err)
			defer p.Close()
			got, err := p.LoadMessages(ctx, "ns", "thread", "run2")
			require.NoError(t, err)
			require.Len(t, got, 2)
			require.Equal(t, "first", got[0].Messages[0].ID)
			require.Equal(t, "second", got[1].Messages[0].ID)
		})
	}
}
