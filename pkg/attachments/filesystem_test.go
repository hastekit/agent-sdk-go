package attachments

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFileStoreSurvivesRestartAndPartitionsByNamespace(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFileStore(dir, FileStoreConfig{})
	require.NoError(t, err)
	ctx := context.Background()
	ref, err := s.Put(ctx, "tenant-a", "thread", Upload{Filename: "../../note.txt", MediaType: "text/plain", Content: bytes.NewBufferString("hello")})
	require.NoError(t, err)
	d, err := s.Lookup(ctx, "tenant-a", "thread", ref)
	require.NoError(t, err)
	require.Equal(t, "note.txt", d.Filename)
	require.EqualValues(t, 5, d.Size)
	require.NoError(t, s.Close())
	s, err = NewFileStore(dir, FileStoreConfig{})
	require.NoError(t, err)
	defer s.Close()
	rd, err := s.Open(ctx, d)
	require.NoError(t, err)
	data, err := io.ReadAll(rd)
	require.NoError(t, err)
	require.NoError(t, rd.Close())
	require.Equal(t, "hello", string(data))
	_, err = s.Lookup(ctx, "tenant-b", "thread", ref)
	require.ErrorIs(t, err, ErrNotFound)
	// A descriptor rewritten to name another namespace does not reach the
	// file it keys.
	forged := d
	forged.Namespace = s.identity + ":other"
	_, err = s.Open(ctx, forged)
	require.ErrorIs(t, err, ErrDenied)
	_, err = s.Lookup(ctx, "", "thread", ref)
	require.ErrorIs(t, err, ErrDenied)
	_, err = s.Lookup(ctx, "tenant-a", "thread", Ref{ID: "../../etc/passwd"})
	require.ErrorIs(t, err, ErrInvalid)
	_, err = s.Lookup(ctx, "tenant-a", "thread", Ref{ID: ref.ID, Version: "changed"})
	require.ErrorIs(t, err, ErrNotFound)
	// A symlink to a file outside the configured root cannot escape os.Root.
	require.NoError(t, os.Remove(filepath.Join(dir, d.Key)))
	outside := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(outside, []byte("secret"), 0600))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, d.Key)))
	_, err = s.Open(ctx, d)
	require.Error(t, err)
}
func TestFileStoreFailedUploadLeavesNoContent(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFileStore(dir, FileStoreConfig{MaxFileBytes: 3})
	require.NoError(t, err)
	defer s.Close()
	ctx := context.Background()
	_, err = s.Put(ctx, "a", "thread", Upload{Filename: "large", MediaType: "text/plain", Content: bytes.NewBufferString("1234")})
	require.ErrorIs(t, err, ErrTooLarge)
	count := 0
	require.NoError(t, filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			count++
		}
		return nil
	}))
	require.Zero(t, count)
}
func TestFileStoreCorruptionIsDetected(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFileStore(dir, FileStoreConfig{})
	require.NoError(t, err)
	defer s.Close()
	ctx := context.Background()
	ref, err := s.Put(ctx, "a", "thread", Upload{Filename: "a", MediaType: "text/plain", Content: bytes.NewBufferString("abc")})
	require.NoError(t, err)
	d, err := s.Lookup(ctx, "a", "thread", ref)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, d.Key), []byte("xyz"), 0600))
	_, err = NewResolver(s, Config{}).Resolve(ctx, "a", "thread", ref)
	require.ErrorIs(t, err, ErrInvalid)
}

func TestFileStoreRejectsMislabeledImage(t *testing.T) {
	s, err := NewFileStore(t.TempDir(), FileStoreConfig{})
	require.NoError(t, err)
	defer s.Close()
	_, err = s.Put(context.Background(), "a", "thread", Upload{Filename: "fake.png", MediaType: "image/png", Content: bytes.NewBufferString("not an image")})
	require.ErrorIs(t, err, ErrInvalid)
}

func TestSessionDirectoryContainsOnlyOriginalFiles(t *testing.T) {
	root := t.TempDir()
	store, err := NewFileStore(root, FileStoreConfig{})
	require.NoError(t, err)
	defer store.Close()
	ref, err := store.Put(t.Context(), "tenant", "thread-one", Upload{Filename: "notes.txt", MediaType: "text/plain", Content: bytes.NewBufferString("first\nsecond\n")})
	require.NoError(t, err)
	require.Equal(t, "thread-one", ref.SessionID)
	dir, err := store.SessionDir(t.Context(), "tenant", "thread-one")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(root, "tenant", "thread-one"), dir)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "notes.txt", entries[0].Name())
	content, err := os.ReadFile(filepath.Join(dir, "notes.txt"))
	require.NoError(t, err)
	require.Equal(t, "first\nsecond\n", string(content))
	_, err = os.Stat(filepath.Join(root, ".metadata", "tenant", ref.ID+".json"))
	require.NoError(t, err)
	metadataEntries, err := os.ReadDir(filepath.Join(root, ".metadata", "tenant"))
	require.NoError(t, err)
	require.Len(t, metadataEntries, 1)
	require.Equal(t, ref.ID+".json", metadataEntries[0].Name())
	for _, removed := range []string{"by-name", "by-id"} {
		_, err := os.Stat(filepath.Join(root, ".metadata", removed))
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	resolver := NewResolver(store, Config{})
	_, err = resolver.Resolve(t.Context(), "tenant", "thread-one", ref)
	require.NoError(t, err)
	_, err = resolver.Resolve(t.Context(), "tenant", "thread-two", ref)
	require.ErrorIs(t, err, ErrDenied, "cache hits still require matching thread scope")
	// Stripping the reference's thread cannot expose another directory's files.
	ref.SessionID = ""
	_, err = store.Lookup(t.Context(), "tenant", "thread-two", ref)
	require.ErrorIs(t, err, ErrDenied)
	for _, invalid := range []string{"", ".", "..", "../other", "a/b", `a\b`, "a\x00b"} {
		_, err = store.SessionDir(t.Context(), "tenant", invalid)
		require.ErrorIs(t, err, ErrDenied)
		_, err = store.SessionDir(t.Context(), invalid, "thread")
		require.ErrorIs(t, err, ErrDenied)
	}
	_, err = store.SessionDir(t.Context(), ".metadata", "thread")
	require.ErrorIs(t, err, ErrDenied)
}

func TestFileStorePublishesCompleteContentWithoutOverwritingOrphans(t *testing.T) {
	root := t.TempDir()
	store, err := NewFileStore(root, FileStoreConfig{})
	require.NoError(t, err)
	defer store.Close()
	dir, err := store.SessionDir(t.Context(), "tenant", "thread")
	require.NoError(t, err)
	// An existing file without UUID metadata must still reserve its own name.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("orphan"), 0600))
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	type result struct {
		ref Ref
		err error
	}
	done := make(chan result, 1)
	go func() {
		ref, err := store.Put(t.Context(), "tenant", "thread", Upload{
			Filename: "notes.txt", MediaType: "text/plain", Content: reader,
		})
		done <- result{ref, err}
	}()
	_, err = writer.Write([]byte("complete content"))
	require.NoError(t, err)
	// The reader has accepted bytes, but the upload has not reached EOF yet.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "notes.txt", entries[0].Name())
	require.NoError(t, writer.Close())
	uploaded := <-done
	require.NoError(t, uploaded.err)
	descriptor, err := store.Lookup(t.Context(), "tenant", "thread", uploaded.ref)
	require.NoError(t, err)
	require.Equal(t, "notes_2.txt", descriptor.StoredFilename)
	data, err := os.ReadFile(filepath.Join(dir, descriptor.StoredFilename))
	require.NoError(t, err)
	require.Equal(t, "complete content", string(data))
	data, err = os.ReadFile(filepath.Join(dir, "notes.txt"))
	require.NoError(t, err)
	require.Equal(t, "orphan", string(data))
}
