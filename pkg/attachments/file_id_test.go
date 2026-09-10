package attachments

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFileIDRoundTrip(t *testing.T) {
	for _, ref := range []Ref{
		{ID: "abc123"},
		{ID: "abc123", Version: "v1"},
		{ID: "image #1%?", Version: "revision/+?&= #%"},
	} {
		id := FileID(ref)
		require.True(t, IsFileID(id))
		decoded, err := RefFromFileID(id)
		require.NoError(t, err)
		require.Equal(t, ref, decoded)
	}
	require.Equal(t, "attachment://abc123?version=v1", FileID(Ref{ID: "abc123", Version: "v1"}))
}

func TestFileIDDistinguishesProviderIDs(t *testing.T) {
	for _, id := range []string{"", "file-abc123", "files/abc123", "https://example.com/file", "attachment-other"} {
		require.False(t, IsFileID(id))
		_, err := RefFromFileID(id)
		require.ErrorIs(t, err, ErrInvalid)
	}
	for _, id := range []string{"attachment:", "attachment://", "attachment://../secret", "attachment://x?unknown=v1", "attachment://x?version=1&version=2", "attachment://x#fragment", "attachment://%zz", "Attachment://x"} {
		require.True(t, IsFileID(id))
		_, err := RefFromFileID(id)
		require.ErrorIs(t, err, ErrInvalid)
	}
}
