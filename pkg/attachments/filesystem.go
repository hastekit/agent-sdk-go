package attachments

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// FileStore implements private, immutable attachment storage on local disk,
// partitioned by namespace. All callers in a namespace may read its files;
// applications with per-file ACLs can wrap Lookup/Open or provide another Store
// implementation. Files are confined using os.Root, including symlinks.
type FileStore struct {
	root     *os.Root
	identity string
	maxBytes int64
}

// UploadStore separates ingestion from read-only LLM resolution. S3 or other
// implementations can implement both Store and UploadStore.
type UploadStore interface {
	Store
	Put(ctx context.Context, namespace string, upload Upload) (Ref, error)
}

type Upload struct {
	Filename  string
	MediaType string
	Content   io.Reader
}

type FileStoreConfig struct {
	MaxFileBytes int64 // zero: 20 MiB
}

type fileMetadata struct {
	Filename  string `json:"filename"`
	MediaType string `json:"media_type"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
}

func NewFileStore(dir string, cfg FileStoreConfig) (*FileStore, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	if cfg.MaxFileBytes <= 0 {
		cfg.MaxFileBytes = 20 << 20
	}
	return &FileStore{root: root, identity: abs, maxBytes: cfg.MaxFileBytes}, nil
}
func (s *FileStore) Close() error { return s.root.Close() }

// dir is the directory a namespace's files live in. An empty namespace names
// nothing, so it is denied rather than given a directory of its own.
func (s *FileStore) dir(ctx context.Context, namespace string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if namespace == "" {
		return "", ErrDenied
	}
	sum := sha256.Sum256([]byte(namespace))
	return hex.EncodeToString(sum[:]), nil
}
func validID(id string) bool {
	b, err := hex.DecodeString(id)
	return err == nil && len(b) == 16 && id == strings.ToLower(id)
}

func (s *FileStore) Put(ctx context.Context, namespace string, in Upload) (Ref, error) {
	ns, err := s.dir(ctx, namespace)
	if err != nil {
		return Ref{}, err
	}
	media, _, err := mime.ParseMediaType(in.MediaType)
	if err != nil || in.Content == nil {
		return Ref{}, fmt.Errorf("%w: upload requires content and media type", ErrInvalid)
	}
	if err := s.root.Mkdir(ns, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return Ref{}, err
	}
	var idBytes [16]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return Ref{}, err
	}
	id := hex.EncodeToString(idBytes[:])
	key := ns + "/" + id
	f, err := s.root.OpenFile(key+".blob", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return Ref{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = s.root.Remove(key + ".blob")
			_ = s.root.Remove(key + ".json")
		}
	}()
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, hash), io.LimitReader(contextReader{ctx, in.Content}, s.maxBytes+1))
	if copyErr == nil && strings.HasPrefix(media, "image/") {
		header := make([]byte, 512)
		nr, readErr := f.ReadAt(header, 0)
		if readErr != nil && readErr != io.EOF {
			copyErr = readErr
		} else if http.DetectContentType(header[:nr]) != media {
			copyErr = fmt.Errorf("%w: image media type does not match content", ErrInvalid)
		}
	}
	if copyErr == nil {
		copyErr = f.Sync()
	}
	closeErr := f.Close()
	if copyErr != nil {
		return Ref{}, copyErr
	}
	if closeErr != nil {
		return Ref{}, closeErr
	}
	if n > s.maxBytes {
		return Ref{}, ErrTooLarge
	}
	if n == 0 {
		return Ref{}, fmt.Errorf("%w: empty file", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return Ref{}, err
	}
	meta := fileMetadata{filepath.Base(in.Filename), media, n, hex.EncodeToString(hash.Sum(nil))}
	// Metadata is committed last. An interrupted write is never returned as a
	// usable reference. IDs are random and clients only receive them on success.
	mf, err := s.root.OpenFile(key+".json", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return Ref{}, err
	}
	writeErr := json.NewEncoder(mf).Encode(meta)
	if writeErr == nil {
		writeErr = mf.Sync()
	}
	closeErr = mf.Close()
	if writeErr != nil {
		return Ref{}, writeErr
	}
	if closeErr != nil {
		return Ref{}, closeErr
	}
	committed = true
	return Ref{ID: id, Version: meta.SHA256}, nil
}

func (s *FileStore) Lookup(ctx context.Context, namespace string, ref Ref) (Descriptor, error) {
	ns, err := s.dir(ctx, namespace)
	if err != nil {
		return Descriptor{}, err
	}
	if !validID(ref.ID) {
		return Descriptor{}, ErrInvalid
	}
	key := ns + "/" + ref.ID
	f, err := s.root.Open(key + ".json")
	if errors.Is(err, os.ErrNotExist) {
		return Descriptor{}, ErrNotFound
	}
	if err != nil {
		return Descriptor{}, err
	}
	defer f.Close()
	var m fileMetadata
	if err := json.NewDecoder(io.LimitReader(f, 64<<10)).Decode(&m); err != nil {
		return Descriptor{}, fmt.Errorf("%w: unreadable file metadata", ErrInvalid)
	}
	if ref.Version != "" && ref.Version != m.SHA256 {
		return Descriptor{}, ErrNotFound
	}
	return Descriptor{Namespace: s.identity + ":" + ns, Key: key, Version: m.SHA256, MediaType: m.MediaType, Size: m.Size, Filename: m.Filename, SHA256: m.SHA256}, nil
}

// Open serves the object a Lookup described. The descriptor names the
// namespace's directory, and a key outside it is refused.
func (s *FileStore) Open(ctx context.Context, d Descriptor) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ns, ok := strings.CutPrefix(d.Namespace, s.identity+":")
	prefix := ns + "/"
	if !ok || ns == "" || !strings.HasPrefix(d.Key, prefix) || !validID(strings.TrimPrefix(d.Key, prefix)) {
		return nil, ErrDenied
	}
	f, err := s.root.Open(d.Key + ".blob")
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &contextReadCloser{contextReader{ctx, f}, f}, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

type contextReadCloser struct {
	contextReader
	closer io.Closer
}

func (r *contextReadCloser) Close() error { return r.closer.Close() }

var _ UploadStore = (*FileStore)(nil)
