package mcpclient

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

// Credentials survive reopening and remain isolated even when subjects contain path-like characters.
func TestFileCredentialStorePersistenceAndIsolation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "credentials")
	store, err := NewFileCredentialStore(dir)
	require.NoError(t, err)
	keys := [][2]string{{"user/a", "gmail"}, {"user", "a/gmail"}, {"user/a", "calendar"}, {"../user", "../gmail"}}
	for i, key := range keys {
		token := &oauth2.Token{AccessToken: "access-" + key[0] + key[1], RefreshToken: "refresh", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour).UTC()}
		require.NoError(t, store.Save(t.Context(), key[0], key[1], token))
		got, err := store.Load(t.Context(), key[0], key[1])
		require.NoError(t, err)
		require.Equal(t, token, got, "record %d", i)
	}
	require.NoError(t, store.Close())

	// Every record is a private file beneath a private directory, independent of subject syntax.
	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0700), info.Mode().Perm())
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, len(keys))
	for _, entry := range entries {
		info, err := entry.Info()
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0600), info.Mode().Perm())
		require.Regexp(t, `^[0-9a-f]{64}\.json$`, entry.Name())
	}

	// A restart sees the same records; deleting one user/server leaves all other pairs intact.
	store, err = NewFileCredentialStore(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	for _, key := range keys {
		_, err := store.Load(t.Context(), key[0], key[1])
		require.NoError(t, err)
	}
	require.NoError(t, store.Delete(t.Context(), "user/a", "gmail"))
	_, err = store.Load(t.Context(), "user/a", "gmail")
	require.ErrorIs(t, err, ErrCredentialNotFound)
	_, err = store.Load(t.Context(), "user/a", "calendar")
	require.NoError(t, err)
}

// Invalid or cancelled writes preserve the previous valid token.
func TestFileCredentialStoreRejectsInvalidWrites(t *testing.T) {
	store, err := NewFileCredentialStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	token := &oauth2.Token{AccessToken: "original"}
	require.NoError(t, store.Save(t.Context(), "user", "gmail", token))
	require.Error(t, store.Save(t.Context(), "", "gmail", token))
	require.Error(t, store.Save(t.Context(), "user", "gmail", &oauth2.Token{}))

	// Cancellation is checked before replacing any existing credential.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, store.Save(ctx, "user", "gmail", &oauth2.Token{AccessToken: "replacement"}), context.Canceled)
	got, err := store.Load(t.Context(), "user", "gmail")
	require.NoError(t, err)
	require.Equal(t, "original", got.AccessToken)
}
