package attachments

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFileIDRoundTrip(t *testing.T) {
	for _, ref := range []Ref{
		{ID: "abc123"},
		{ID: "abc123", Version: "v1"},
		{ID: "abc123", SessionID: "thread #1%?&", Version: "v1"},
		{ID: "cc2c80ea-e99f-41bb-9b5f-de9f6441b9de", Version: "revision/+?&= #%"},
	} {
		id := FileID(ref)
		require.True(t, IsFileID(id))
		decoded, err := RefFromFileID(id)
		require.NoError(t, err)
		require.Equal(t, Ref{ID: ref.ID}, decoded)
	}
	require.Equal(t, "attachment://abc123", FileID(Ref{ID: "abc123", Version: "v1"}))
}

func TestFileIDDistinguishesProviderIDs(t *testing.T) {
	for _, id := range []string{"", "file-abc123", "files/abc123", "https://example.com/file", "attachment-other"} {
		require.False(t, IsFileID(id))
		_, err := RefFromFileID(id)
		require.ErrorIs(t, err, ErrInvalid)
	}
	for _, id := range []string{"attachment:", "attachment://", "attachment://../secret", "attachment://x?unknown=v1", "attachment://x?version=1&version=2", "attachment://x#fragment", "attachment://%zz", "Attachment://x", "attachment://x?session_id=../other", "attachment://x?session_id=one&session_id=two"} {
		require.True(t, IsFileID(id))
		_, err := RefFromFileID(id)
		require.ErrorIs(t, err, ErrInvalid)
	}
}
