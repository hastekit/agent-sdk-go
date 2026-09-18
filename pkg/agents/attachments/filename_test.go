package attachments

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAttachmentFilenames(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"report.pdf", "report.pdf"}, {"../../report.pdf", "report.pdf"},
		{`C:\uploads\report.pdf`, "report.pdf"}, {"Screenshot at 6.11.53\u202fPM.png", "Screenshot at 6.11.53 PM.png"},
		{"résumé.pdf", "résumé.pdf"}, {"", "attachment"}, {"..", "attachment"},
		{"-options.txt", "attachment_-options.txt"}, {"$(touch evil).txt", "__touch evil_.txt"},
	} {
		require.Equal(t, tc.want, attachmentFilename(tc.input))
		require.True(t, validFilename(tc.want))
	}
	name := attachmentFilename(strings.Repeat("é", 200) + ".pdf")
	require.LessOrEqual(t, len(name), 180)
	require.True(t, validFilename(name))
	require.True(t, validFilename(numberedFilename(name, 123)))
	for _, id := range []string{"cc2c80ea-e99f-41bb-9b5f-de9f6441b9de", "e6eb575c-912b-4c32-a37c-d49fb729df66"} {
		ref, err := ParseRef(id)
		require.NoError(t, err)
		canonical, err := ParseRef(FileID(Ref{ID: id, SessionID: "thread", Version: "v1"}))
		require.NoError(t, err)
		require.Equal(t, ref, canonical)
	}
	for _, name := range []string{"", "..", "../report.pdf", "/report.pdf", "https://host/file", "a\x00b"} {
		_, err := ParseRef(name)
		require.ErrorIs(t, err, ErrInvalid)
	}
}

func TestFilenameCollisionsAndResolution(t *testing.T) {
	fs, err := NewFileStore(t.TempDir(), FileStoreConfig{})
	require.NoError(t, err)
	defer fs.Close()
	s3, err := NewS3Store(&testS3{objects: map[string]storedS3Object{}}, S3StoreConfig{Bucket: "test"})
	require.NoError(t, err)
	for _, store := range []UploadStore{fs, s3} {
		for i, filename := range []string{"report.pdf", "report_2.pdf", "report_3.pdf"} {
			content := fmt.Sprintf("content %d", i)
			ref, err := store.Put(t.Context(), "tenant", "thread", Upload{Filename: "report.pdf", MediaType: "text/plain", Content: strings.NewReader(content)})
			require.NoError(t, err)
			require.True(t, validID(ref.ID))
			// A tool can resolve the exact short name shown to the model.
			plain, err := ParseRef(FileID(ref))
			require.NoError(t, err)
			descriptor, err := store.Lookup(t.Context(), "tenant", "thread", plain)
			require.NoError(t, err)
			require.Equal(t, "report.pdf", descriptor.Filename)
			require.Equal(t, filename, descriptor.StoredFilename)
			reader, err := store.Open(t.Context(), descriptor)
			require.NoError(t, err)
			data, err := io.ReadAll(reader)
			require.NoError(t, err)
			require.NoError(t, reader.Close())
			require.Equal(t, content, string(data))
		}
		ref, err := store.Put(t.Context(), "tenant", "other-thread", Upload{Filename: "report.pdf", MediaType: "text/plain", Content: strings.NewReader("other")})
		require.NoError(t, err)
		d, err := store.Lookup(t.Context(), "tenant", "other-thread", ref)
		require.NoError(t, err)
		require.Equal(t, "report.pdf", d.StoredFilename)
	}
}

func TestConcurrentFilenameReservationSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	first, err := NewFileStore(root, FileStoreConfig{})
	require.NoError(t, err)
	second, err := NewFileStore(root, FileStoreConfig{})
	require.NoError(t, err)
	const count = 12
	type result struct {
		ref     Ref
		err     error
		content string
	}
	results := make(chan result, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store := first
			if i%2 != 0 {
				store = second
			}
			content := fmt.Sprintf("file %d", i)
			ref, err := store.Put(t.Context(), "tenant", "thread", Upload{Filename: "notes.txt", MediaType: "text/plain", Content: bytes.NewBufferString(content)})
			results <- result{ref, err, content}
		}(i)
	}
	wg.Wait()
	close(results)
	seen := map[string]bool{}
	for result := range results {
		require.NoError(t, result.err)
		d, err := first.Lookup(t.Context(), "tenant", "thread", result.ref)
		require.NoError(t, err)
		require.False(t, seen[d.StoredFilename])
		seen[d.StoredFilename] = true
		data, err := os.ReadFile(filepath.Join(root, d.Key))
		require.NoError(t, err)
		require.Equal(t, result.content, string(data))
	}
	require.True(t, seen["notes.txt"])
	require.True(t, seen["notes_12.txt"])
	require.NoError(t, first.Close())
	require.NoError(t, second.Close())
	reopened, err := NewFileStore(root, FileStoreConfig{})
	require.NoError(t, err)
	defer reopened.Close()
	ref, err := reopened.Put(t.Context(), "tenant", "thread", Upload{Filename: "notes.txt", MediaType: "text/plain", Content: strings.NewReader("later")})
	require.NoError(t, err)
	d, err := reopened.Lookup(t.Context(), "tenant", "thread", ref)
	require.NoError(t, err)
	require.Equal(t, "notes_13.txt", d.StoredFilename)
}

func TestUUIDReferenceAndOptionalMountPath(t *testing.T) {
	for _, mount := range []string{"", "/mnt/user-data/uploads/"} {
		local, err := NewFileStore(t.TempDir(), FileStoreConfig{MountPath: mount})
		require.NoError(t, err)
		t.Cleanup(func() { _ = local.Close() })
		remote, err := NewS3Store(&testS3{objects: map[string]storedS3Object{}}, S3StoreConfig{Bucket: "test", MountPath: mount})
		require.NoError(t, err)
		for _, store := range []UploadStore{local, remote} {
			for i := 1; i <= 2; i++ {
				ref, err := store.Put(t.Context(), "tenant", "thread", Upload{Filename: "../report.json", MediaType: "application/json", Content: strings.NewReader(`{"ok":true}`)})
				require.NoError(t, err)
				require.True(t, validID(ref.ID))
				require.Equal(t, "attachment://"+ref.ID, FileID(ref))
				parsed, err := ParseRef(FileID(ref))
				require.NoError(t, err)
				d, err := store.Lookup(t.Context(), "tenant", "thread", parsed)
				require.NoError(t, err)
				name := numberedFilename("report.json", i)
				require.Equal(t, name, d.StoredFilename)
				if mount == "" {
					require.Empty(t, d.MountPath)
				} else {
					require.Equal(t, "/mnt/user-data/uploads/"+name, d.MountPath)
				}
				_, err = store.Lookup(t.Context(), "tenant", "other-thread", parsed)
				require.ErrorIs(t, err, ErrDenied)
				_, err = store.Lookup(t.Context(), "other-tenant", "thread", parsed)
				require.ErrorIs(t, err, ErrNotFound)
				downloaded, err := store.(ReferenceStore).LookupReference(t.Context(), "tenant", parsed)
				require.NoError(t, err)
				require.Equal(t, d, downloaded)
			}
		}
	}
	for _, invalid := range []string{"relative/uploads", `C:\uploads`, "/mnt/\x00uploads"} {
		_, err := NewFileStore(t.TempDir(), FileStoreConfig{MountPath: invalid})
		require.ErrorIs(t, err, ErrInvalid)
		_, err = NewS3Store(&testS3{}, S3StoreConfig{Bucket: "test", MountPath: invalid})
		require.ErrorIs(t, err, ErrInvalid)
	}
}
