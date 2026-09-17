package attachments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// FileStore implements private, immutable attachment storage on local disk,
// partitioned by namespace and session. The host authorizes both scopes;
// applications with per-file ACLs can wrap Lookup/Open or provide another Store
// implementation. Files are confined using os.Root, including symlinks.
type FileStore struct {
	root      *os.Root
	identity  string
	maxBytes  int64
	mountPath string
}

// UploadStore separates ingestion from read-only LLM resolution. S3 or other
// implementations can implement both Store and UploadStore.
type UploadStore interface {
	Store
	Put(ctx context.Context, namespace, sessionID string, upload Upload) (Ref, error)
}

type Upload struct {
	Filename  string
	MediaType string
	Content   io.Reader
}

type FileStoreConfig struct {
	// MountPath is the absolute container path at which SessionDir is mounted.
	// Empty omits filesystem access information from model-facing metadata.
	MountPath    string
	MaxFileBytes int64 // zero: 20 MiB
}

type fileMetadata struct {
	SessionID      string `json:"session_id"`
	StoredFilename string `json:"stored_filename"`
	Filename       string `json:"filename"`
	MediaType      string `json:"media_type"`
	Size           int64  `json:"size"`
	SHA256         string `json:"sha256"`
}

func NewFileStore(dir string, cfg FileStoreConfig) (*FileStore, error) {
	if err := validateMountPath(cfg.MountPath); err != nil {
		return nil, err
	}
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
	return &FileStore{root: root, identity: abs, maxBytes: cfg.MaxFileBytes, mountPath: cfg.MountPath}, nil
}
func (s *FileStore) Close() error { return s.root.Close() }

// dir validates filesystem components before constructing a session-scoped path.
func (s *FileStore) dir(ctx context.Context, namespace, sessionID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !validScopePart(namespace) || namespace == ".metadata" || !validScopePart(sessionID) {
		return "", ErrDenied
	}
	return namespace + "/" + sessionID, nil
}

func validScopePart(value string) bool {
	return value != "" && value != "." && value != ".." && !strings.ContainsAny(value, "/\\\x00")
}

// SessionDir returns the directory to bind-mount into a sandbox, containing only
// this session's files under their sanitized, collision-safe names. Mount it read-only to preserve
// immutable attachments. Namespace/thread authorization belongs to the caller.
func (s *FileStore) SessionDir(ctx context.Context, namespace, sessionID string) (string, error) {
	dir, err := s.dir(ctx, namespace, sessionID)
	if err != nil {
		return "", err
	}
	if err := s.root.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return filepath.Join(s.identity, dir), nil
}

func (s *FileStore) Put(ctx context.Context, namespace, sessionID string, in Upload) (Ref, error) {
	ns, err := s.dir(ctx, namespace, sessionID)
	if err != nil {
		return Ref{}, err
	}
	media, _, err := mime.ParseMediaType(in.MediaType)
	if err != nil || in.Content == nil {
		return Ref{}, fmt.Errorf("%w: upload requires content and media type", ErrInvalid)
	}
	if err := s.root.MkdirAll(ns, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return Ref{}, err
	}
	base := attachmentFilename(in.Filename)
	uuidID := uuid.NewString()
	indexKey := ".metadata/" + namespace + "/" + uuidID + ".json"
	if err := s.root.MkdirAll(".metadata/"+namespace, 0700); err != nil {
		return Ref{}, err
	}
	// Stage bytes outside the mounted session directory. Publishing a hard link
	// later claims the final filename atomically without replacing existing files.
	tempKey := ".metadata/" + namespace + "/" + uuidID + ".upload"
	f, err := s.root.OpenFile(tempKey, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return Ref{}, err
	}
	var id, key string
	committed, published, indexed := false, false, false
	defer func() {
		_ = f.Close()
		_ = s.root.Remove(tempKey)
		if !committed {
			if published {
				_ = s.root.Remove(key)
			}
			if indexed {
				_ = s.root.Remove(indexKey)
			}
		}
	}()
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, hash), io.LimitReader(contextReader{ctx, in.Content}, s.maxBytes+1))
	if copyErr == nil && InlineImageMediaType(media) {
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
	// Link fails with ErrExist when another upload (or an orphaned file) already
	// owns the name. Readers only ever see the fully written and validated bytes.
	for number := 1; ; number++ {
		if err := ctx.Err(); err != nil {
			return Ref{}, err
		}
		id = numberedFilename(base, number)
		key = ns + "/" + id
		if err := s.root.Link(tempKey, key); errors.Is(err, os.ErrExist) {
			continue
		} else if err != nil {
			return Ref{}, err
		}
		published = true
		break
	}
	meta := fileMetadata{SessionID: sessionID, StoredFilename: id, Filename: originalFilename(in.Filename), MediaType: media, Size: n, SHA256: hex.EncodeToString(hash.Sum(nil))}
	index, err := s.root.OpenFile(indexKey, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return Ref{}, err
	}
	indexed = true
	indexErr := json.NewEncoder(index).Encode(meta)
	if indexErr == nil {
		indexErr = index.Sync()
	}
	closeIndexErr := index.Close()
	if indexErr != nil {
		return Ref{}, indexErr
	}
	if closeIndexErr != nil {
		return Ref{}, closeIndexErr
	}
	committed = true
	return Ref{ID: uuidID, SessionID: sessionID, Version: meta.SHA256}, nil
}

func (s *FileStore) Lookup(ctx context.Context, namespace, sessionID string, ref Ref) (Descriptor, error) {
	if _, err := s.dir(ctx, namespace, sessionID); err != nil {
		return Descriptor{}, err
	}
	if ref.SessionID != "" && ref.SessionID != sessionID {
		return Descriptor{}, ErrDenied
	}
	d, err := s.LookupReference(ctx, namespace, ref)
	if err != nil {
		return Descriptor{}, err
	}
	if d.Namespace != s.identity+":"+namespace+"/"+sessionID {
		return Descriptor{}, ErrDenied
	}
	return d, nil
}

func (s *FileStore) LookupReference(ctx context.Context, namespace string, ref Ref) (Descriptor, error) {
	if err := ctx.Err(); err != nil {
		return Descriptor{}, err
	}
	if !validScopePart(namespace) || namespace == ".metadata" {
		return Descriptor{}, ErrDenied
	}
	if !validID(ref.ID) {
		return Descriptor{}, ErrInvalid
	}
	f, err := s.root.Open(".metadata/" + namespace + "/" + ref.ID + ".json")
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
	if !validScopePart(m.SessionID) || !validFilename(m.StoredFilename) {
		return Descriptor{}, ErrInvalid
	}
	if ref.SessionID != "" && ref.SessionID != m.SessionID {
		return Descriptor{}, ErrDenied
	}
	if ref.Version != "" && ref.Version != m.SHA256 {
		return Descriptor{}, ErrNotFound
	}
	ns := namespace + "/" + m.SessionID
	return Descriptor{Namespace: s.identity + ":" + ns, Key: ns + "/" + m.StoredFilename, Version: m.SHA256, MediaType: m.MediaType, Size: m.Size, Filename: m.Filename, StoredFilename: m.StoredFilename, MountPath: mountedPath(s.mountPath, m.StoredFilename), SHA256: m.SHA256}, nil
}

// Open serves the object a Lookup described. The descriptor names the
// namespace's directory, and a key outside it is refused.
func (s *FileStore) Open(ctx context.Context, d Descriptor) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ns, ok := strings.CutPrefix(d.Namespace, s.identity+":")
	prefix := ns + "/"
	parts := strings.Split(ns, "/")
	if !ok || len(parts) != 2 || !validScopePart(parts[0]) || parts[0] == ".metadata" || !validScopePart(parts[1]) || !strings.HasPrefix(d.Key, prefix) || !validFilename(strings.TrimPrefix(d.Key, prefix)) {
		return nil, ErrDenied
	}
	f, err := s.root.Open(d.Key)
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
