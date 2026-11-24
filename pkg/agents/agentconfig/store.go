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
	"sort"
	"strings"
	"sync"
)

// Store lists and reads the agents namespaces create at run time. Built-in
// agents are not stored: they are configs in code, given to NewRegistry.
//
// Every call names a namespace, which is never empty. List returns the
// namespace's configs ordered by name; namespaces hold an agent per thing a
// user built, not per request, so a listing is not paged. Get returns
// ErrNotFound for a name the namespace lacks. Implementations must be safe for
// concurrent callers and honor cancellation.
type Store interface {
	Get(ctx context.Context, namespace, name string) (Config, error)
	List(ctx context.Context, namespace string) ([]Config, error)
}

// MutableStore is a Store that also saves configs. Implementations must
// isolate namespaces, replace a config atomically, make Delete idempotent, and
// be safe for concurrent callers. Put returns the config as stored.
type MutableStore interface {
	Store
	Put(ctx context.Context, namespace string, config Config) (Config, error)
	Delete(ctx context.Context, namespace, name string) error
}

// checkNamespace rejects an empty or blank namespace: stores hold namespaces'
// own agents only.
func checkNamespace(ctx context.Context, namespace string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(namespace) == "" {
		return fmt.Errorf("%w: namespace required", ErrInvalid)
	}
	return nil
}

// sortedClones copies configs in name order, so nothing a caller holds can
// change what was stored.
func sortedClones(configs []Config) ([]Config, error) {
	result := make([]Config, 0, len(configs))
	for _, c := range configs {
		copy, err := clone(c)
		if err != nil {
			return nil, err
		}
		result = append(result, copy)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
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

func (s *MemoryStore) List(ctx context.Context, namespace string) ([]Config, error) {
	if err := checkNamespace(ctx, namespace); err != nil {
		return nil, err
	}
	s.mu.RLock()
	configs := make([]Config, 0, len(s.namespaces[namespace]))
	for _, c := range s.namespaces[namespace] {
		configs = append(configs, c)
	}
	s.mu.RUnlock()
	return sortedClones(configs)
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

// List reads the namespace's directory whole and orders it by name, since file
// names are hashes.
func (s *FileStore) List(ctx context.Context, namespace string) ([]Config, error) {
	dir, err := s.dir(ctx, namespace)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return []Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	configs := []Config{}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		c, err := readConfigFile(filepath.Join(dir, entry.Name()))
		if errors.Is(err, ErrNotFound) {
			continue // removed while listing
		}
		if err != nil {
			return nil, err
		}
		configs = append(configs, c)
	}
	sort.Slice(configs, func(i, j int) bool { return configs[i].Name < configs[j].Name })
	return configs, nil
}

var (
	_ MutableStore = (*MemoryStore)(nil)
	_ MutableStore = (*FileStore)(nil)
)
