package agentconfig

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
)

// Store lists and reads agent configs by namespace. It is everything a Builder
// needs, so built-ins and custom backends implement only this. It mirrors
// skills.Store.
//
// The empty namespace holds built-in agents: developer-owned, runnable from
// every namespace, and winning over a namespace's own config with the same
// name. Any other namespace holds that namespace's own agents. List pages
// through one namespace only; Get returns ErrNotFound for a name the namespace
// lacks. Implementations must be safe for concurrent callers and honor
// cancellation.
type Store interface {
	Get(ctx context.Context, namespace, name string) (Config, error)
	List(ctx context.Context, namespace string, opts ListOptions) (Page, error)
}

// MutableStore is a Store that also saves configs. Implementations must
// isolate namespaces, replace a config atomically, make Delete idempotent, and
// be safe for concurrent callers. Put returns the config as stored.
//
// Writing the empty namespace changes every namespace's built-in agents, so
// only trusted server code may do it. NewHandler refuses it, whatever
// namespace the request resolves to.
type MutableStore interface {
	Store
	Put(ctx context.Context, namespace string, config Config) (Config, error)
	Delete(ctx context.Context, namespace, name string) error
}

// ListOptions pages a listing. Limit defaults to 50 and is capped at 200.
type ListOptions struct {
	Limit  int
	Cursor string
}

// Page is one page of a namespace's configs, ordered by name.
type Page struct {
	Agents     []Config `json:"agents"`
	NextCursor string   `json:"nextCursor,omitempty"`
}

func limit(n int) int {
	if n <= 0 {
		return 50
	}
	if n > 200 {
		return 200
	}
	return n
}

// checkNamespace rejects a blank but nonempty namespace, so it cannot pass for
// the built-in one.
func checkNamespace(ctx context.Context, namespace string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if namespace != "" && strings.TrimSpace(namespace) == "" {
		return fmt.Errorf("%w: blank namespace", ErrInvalid)
	}
	return nil
}

// pageOf pages through configs in name order. The cursor is the last name
// returned, so it stays valid while configs are added or removed.
func pageOf(configs []Config, opts ListOptions) (Page, error) {
	sorted := slices.Clone(configs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	page := Page{Agents: []Config{}}
	for _, c := range sorted {
		if opts.Cursor != "" && c.Name <= opts.Cursor {
			continue
		}
		if len(page.Agents) == limit(opts.Limit) {
			page.NextCursor = page.Agents[len(page.Agents)-1].Name
			break
		}
		copy, err := clone(c)
		if err != nil {
			return Page{}, err
		}
		page.Agents = append(page.Agents, copy)
	}
	return page, nil
}

// inlineStore serves configs written in code as built-ins.
type inlineStore struct {
	configs map[string]Config
}

// NewInlineStore serves configs defined in code, or loaded by the application
// from its own files, as built-in agents: under the empty namespace and no
// other. It validates and copies them, and never accepts writes.
func NewInlineStore(configs ...Config) (Store, error) {
	store := &inlineStore{configs: make(map[string]Config, len(configs))}
	for _, c := range configs {
		if err := Validate(c); err != nil {
			return nil, err
		}
		if _, exists := store.configs[c.Name]; exists {
			return nil, fmt.Errorf("%w: duplicate built-in agent %q", ErrInvalid, c.Name)
		}
		copy, err := clone(c)
		if err != nil {
			return nil, err
		}
		store.configs[c.Name] = copy
	}
	return store, nil
}

func (s *inlineStore) Get(ctx context.Context, namespace, name string) (Config, error) {
	if err := checkNamespace(ctx, namespace); err != nil {
		return Config{}, err
	}
	c, ok := s.configs[name]
	if namespace != "" || !ok {
		return Config{}, ErrNotFound
	}
	return clone(c)
}

func (s *inlineStore) List(ctx context.Context, namespace string, opts ListOptions) (Page, error) {
	if err := checkNamespace(ctx, namespace); err != nil {
		return Page{}, err
	}
	if namespace != "" {
		return Page{Agents: []Config{}}, nil
	}
	configs := make([]Config, 0, len(s.configs))
	for _, c := range s.configs {
		configs = append(configs, c)
	}
	return pageOf(configs, opts)
}

// MemoryStore keeps configs in process memory, for tests and single-process
// servers. Its zero value is not usable; call NewMemoryStore.
type MemoryStore struct {
	mu         sync.RWMutex
	namespaces map[string]map[string]Config
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{namespaces: map[string]map[string]Config{}}
}

func (s *MemoryStore) Put(ctx context.Context, namespace string, c Config) (Config, error) {
	if err := checkNamespace(ctx, namespace); err != nil {
		return Config{}, err
	}
	if err := Validate(c); err != nil {
		return Config{}, err
	}
	stored, err := clone(c)
	if err != nil {
		return Config{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.namespaces[namespace] == nil {
		s.namespaces[namespace] = map[string]Config{}
	}
	s.namespaces[namespace][c.Name] = stored
	return clone(stored)
}

func (s *MemoryStore) Get(ctx context.Context, namespace, name string) (Config, error) {
	if err := checkNamespace(ctx, namespace); err != nil {
		return Config{}, err
	}
	s.mu.RLock()
	c, ok := s.namespaces[namespace][name]
	s.mu.RUnlock()
	if !ok {
		return Config{}, ErrNotFound
	}
	return clone(c)
}

func (s *MemoryStore) Delete(ctx context.Context, namespace, name string) error {
	if err := checkNamespace(ctx, namespace); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.namespaces[namespace], name)
	return nil
}

func (s *MemoryStore) List(ctx context.Context, namespace string, opts ListOptions) (Page, error) {
	if err := checkNamespace(ctx, namespace); err != nil {
		return Page{}, err
	}
	s.mu.RLock()
	configs := make([]Config, 0, len(s.namespaces[namespace]))
	for _, c := range s.namespaces[namespace] {
		configs = append(configs, c)
	}
	s.mu.RUnlock()
	return pageOf(configs, opts)
}

// FileStore keeps one JSON file per config, in a directory per namespace. The
// directory is application-owned and must not be writable by untrusted
// processes. Rename makes replacement atomic.
type FileStore struct {
	root string
}

func NewFileStore(directory string) (*FileStore, error) {
	if strings.TrimSpace(directory) == "" {
		return nil, fmt.Errorf("%w: directory required", ErrInvalid)
	}
	root, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &FileStore{root: root}, nil
}

// Hashed path segments keep namespaces and names, which come from users, from
// ever being read as paths.
func hashed(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func (s *FileStore) dir(ctx context.Context, namespace string) (string, error) {
	if err := checkNamespace(ctx, namespace); err != nil {
		return "", err
	}
	return filepath.Join(s.root, hashed(namespace)), nil
}

func (s *FileStore) path(ctx context.Context, namespace, name string) (string, error) {
	dir, err := s.dir(ctx, namespace)
	if err != nil {
		return "", err
	}
	if !validName(name) {
		return "", fmt.Errorf("%w: name %q", ErrInvalid, name)
	}
	return filepath.Join(dir, hashed(name)+".json"), nil
}

func (s *FileStore) Put(ctx context.Context, namespace string, c Config) (Config, error) {
	if err := Validate(c); err != nil {
		return Config{}, err
	}
	target, err := s.path(ctx, namespace, c.Name)
	if err != nil {
		return Config{}, err
	}
	data, err := json.Marshal(c)
	if err != nil {
		return Config{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Config{}, err
	}
	f, err := os.CreateTemp(dir, ".config-")
	if err != nil {
		return Config{}, err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return Config{}, err
	}
	if err := ctx.Err(); err != nil {
		return Config{}, err
	}
	if err := os.Rename(f.Name(), target); err != nil {
		return Config{}, err
	}
	return decode(data)
}

func (s *FileStore) Get(ctx context.Context, namespace, name string) (Config, error) {
	target, err := s.path(ctx, namespace, name)
	if errors.Is(err, ErrInvalid) && !validName(name) {
		return Config{}, ErrNotFound // no stored config can have this name
	}
	if err != nil {
		return Config{}, err
	}
	return readConfigFile(target)
}

func readConfigFile(path string) (Config, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, ErrNotFound
	}
	if err != nil {
		return Config{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxConfigBytes+1))
	if err != nil {
		return Config{}, err
	}
	if len(data) > MaxConfigBytes {
		return Config{}, fmt.Errorf("%w: stored config exceeds %d bytes", ErrInvalid, MaxConfigBytes)
	}
	return decode(data)
}

func (s *FileStore) Delete(ctx context.Context, namespace, name string) error {
	target, err := s.path(ctx, namespace, name)
	if err != nil {
		return err
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// List orders by name. File names are hashes, so a page reads the namespace's
// directory whole; namespaces hold an agent per thing a user built, not per
// request, so this stays small.
func (s *FileStore) List(ctx context.Context, namespace string, opts ListOptions) (Page, error) {
	dir, err := s.dir(ctx, namespace)
	if err != nil {
		return Page{}, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return Page{Agents: []Config{}}, nil
	}
	if err != nil {
		return Page{}, err
	}
	var configs []Config
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return Page{}, err
		}
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		c, err := readConfigFile(filepath.Join(dir, entry.Name()))
		if errors.Is(err, ErrNotFound) {
			continue // removed while listing
		}
		if err != nil {
			return Page{}, err
		}
		configs = append(configs, c)
	}
	return pageOf(configs, opts)
}

// listAll reads every page of one namespace, rejecting cursors that repeat.
func listAll(ctx context.Context, store Store, namespace string) ([]Config, error) {
	var result []Config
	cursor := ""
	seen := map[string]bool{}
	for {
		page, err := store.List(ctx, namespace, ListOptions{Limit: 200, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		result = append(result, page.Agents...)
		if page.NextCursor == "" {
			return result, nil
		}
		if seen[page.NextCursor] {
			return nil, errors.New("agent config store repeated pagination cursor")
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
	}
}

var (
	_ MutableStore = (*MemoryStore)(nil)
	_ MutableStore = (*FileStore)(nil)
)
