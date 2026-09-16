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
	ref, err := s.Put(ctx, "tenant-a", Upload{Filename: "../../note.txt", MediaType: "text/plain", Content: bytes.NewBufferString("hello")})
	require.NoError(t, err)
	d, err := s.Lookup(ctx, "tenant-a", ref)
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
	_, err = s.Lookup(ctx, "tenant-b", ref)
	require.ErrorIs(t, err, ErrNotFound)
	// A descriptor rewritten to name another namespace does not reach the
	// file it keys.
	forged := d
	forged.Namespace = s.identity + ":other"
	_, err = s.Open(ctx, forged)
	require.ErrorIs(t, err, ErrDenied)
	_, err = s.Lookup(ctx, "", ref)
	require.ErrorIs(t, err, ErrDenied)
	_, err = s.Lookup(ctx, "tenant-a", Ref{ID: "../../etc/passwd"})
	require.ErrorIs(t, err, ErrInvalid)
	_, err = s.Lookup(ctx, "tenant-a", Ref{ID: ref.ID, Version: "changed"})
	require.ErrorIs(t, err, ErrNotFound)
	// A symlink to a file outside the configured root cannot escape os.Root.
	require.NoError(t, os.Remove(filepath.Join(dir, d.Key+".blob")))
	outside := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(outside, []byte("secret"), 0600))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, d.Key+".blob")))
	_, err = s.Open(ctx, d)
	require.Error(t, err)
}
func TestFileStoreFailedUploadLeavesNoContent(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFileStore(dir, FileStoreConfig{MaxFileBytes: 3})
	require.NoError(t, err)
	defer s.Close()
	ctx := context.Background()
	_, err = s.Put(ctx, "a", Upload{Filename: "large", MediaType: "text/plain", Content: bytes.NewBufferString("1234")})
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
	ref, err := s.Put(ctx, "a", Upload{Filename: "a", MediaType: "text/plain", Content: bytes.NewBufferString("abc")})
	require.NoError(t, err)
	d, err := s.Lookup(ctx, "a", ref)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, d.Key+".blob"), []byte("xyz"), 0600))
	_, err = NewResolver(s, Config{}).Resolve(ctx, "a", ref)
	require.ErrorIs(t, err, ErrInvalid)
}

func TestFileStoreRejectsMislabeledImage(t *testing.T) {
	s, err := NewFileStore(t.TempDir(), FileStoreConfig{})
	require.NoError(t, err)
	defer s.Close()
	_, err = s.Put(context.Background(), "a", Upload{Filename: "fake.png", MediaType: "image/png", Content: bytes.NewBufferString("not an image")})
	require.ErrorIs(t, err, ErrInvalid)
}
