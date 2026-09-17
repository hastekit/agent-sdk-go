package skills

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FileStore stores one JSON bundle per skill. The directory is application-owned
// and must not be writable by untrusted processes. Rename makes replacement atomic.
type FileStore struct {
	root      string
	listCache listCache
}

func NewFileStore(directory string, opts ...StoreOption) (*FileStore, error) {
	if strings.TrimSpace(directory) == "" {
		return nil, fmt.Errorf("%w: directory required", ErrInvalid)
	}
	root, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	return &FileStore{root: root, listCache: configureStore(opts)}, nil
}
func (s *FileStore) dir(ctx context.Context, ns string) (string, error) {
	key, err := namespaceKey(ctx, ns)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.root, key), nil
}
func (s *FileStore) Put(ctx context.Context, ns string, b Bundle) (Metadata, error) {
	m, err := Validate(b)
	if err != nil {
		return m, err
	}
	dir, err := s.dir(ctx, ns)
	if err != nil {
		return m, err
	}
	key, _ := skillKey(m.Name)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return m, err
	}
	f, err := os.CreateTemp(dir, ".upload-")
	if err != nil {
		return m, err
	}
	defer os.Remove(f.Name())
	err = json.NewEncoder(f).Encode(b)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return m, err
	}
	if closeErr != nil {
		return m, closeErr
	}
	if err = ctx.Err(); err != nil {
		return m, err
	}
	err = os.Rename(f.Name(), filepath.Join(dir, key))
	return m, invalidateList(ctx, s.listCache, s.cacheScope(ns), err)
}
func decodeBundle(r io.Reader) (Bundle, error) {
	var b Bundle
	data, err := io.ReadAll(io.LimitReader(r, 2*MaxBundleBytes+1))
	if err != nil {
		return b, err
	}
	if len(data) > 2*MaxBundleBytes {
		return b, ErrTooLarge
	}
	if err = json.Unmarshal(data, &b); err != nil {
		return b, err
	}
	_, err = Validate(b)
	return b, err
}
func (s *FileStore) Get(ctx context.Context, ns, name string) (Bundle, error) {
	dir, err := s.dir(ctx, ns)
	if err != nil {
		return Bundle{}, err
	}
	key, err := skillKey(name)
	if err != nil {
		return Bundle{}, err
	}
	f, err := os.Open(filepath.Join(dir, key))
	if errors.Is(err, fs.ErrNotExist) {
		return Bundle{}, ErrNotFound
	}
	if err != nil {
		return Bundle{}, err
	}
	defer f.Close()
	return decodeBundle(f)
}
func (s *FileStore) Delete(ctx context.Context, ns, name string) error {
	dir, err := s.dir(ctx, ns)
	if err != nil {
		return err
	}
	key, err := skillKey(name)
	if err != nil {
		return err
	}
	err = os.Remove(filepath.Join(dir, key))
	if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	return invalidateList(ctx, s.listCache, s.cacheScope(ns), err)
}
func (s *FileStore) List(ctx context.Context, ns string, opts ListOptions) (Page, error) {
	if _, err := namespaceKey(ctx, ns); err != nil {
		return Page{}, err
	}
	return s.listCache.list(ctx, s.cacheScope(ns), opts, func() (Page, error) { return s.list(ctx, ns, opts) })
}

func (s *FileStore) list(ctx context.Context, ns string, opts ListOptions) (Page, error) {
	page := Page{Skills: []Metadata{}}
	dir, err := s.dir(ctx, ns)
	if err != nil {
		return page, err
	}
	after := ""
	if opts.Cursor != "" {
		data, e := base64.RawURLEncoding.DecodeString(opts.Cursor)
		if e != nil {
			return page, fmt.Errorf("%w: cursor", ErrInvalid)
		}
		after = string(data)
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return page, nil
	}
	if err != nil {
		return page, err
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".json") && e.Name() > after {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for i, key := range names {
		if err := ctx.Err(); err != nil {
			return page, err
		}
		f, err := os.Open(filepath.Join(dir, key))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return page, err
		}
		b, err := decodeBundle(f)
		f.Close()
		if err != nil {
			return page, err
		}
		m, err := Validate(b)
		if err != nil {
			return page, err
		}
		page.Skills = append(page.Skills, m)
		if len(page.Skills) == limit(opts.Limit) {
			if i+1 < len(names) {
				page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(key))
			}
			break
		}
	}
	return page, nil
}

func (s *FileStore) cacheScope(ns string) string { return "file:" + s.root + "\x00" + ns }
