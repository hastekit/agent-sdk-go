package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// memoryConfigStore holds global and namespace-owned definitions in one process.
type memoryConfigStore struct {
	mu      sync.RWMutex
	configs map[serverConfigKey]ServerConfig
}

type serverConfigKey struct{ Namespace, Name string }

func newMemoryConfigStore() *memoryConfigStore {
	return &memoryConfigStore{configs: make(map[serverConfigKey]ServerConfig)}
}

// Put writes to the caller-authorized namespace, ignoring any namespace on the supplied config.
func (s *memoryConfigStore) Put(ctx context.Context, namespace string, config ServerConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	config.Namespace = namespace
	if err := validateServerConfig(config); err != nil {
		return err
	}
	copy, err := cloneServerConfig(config)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.configs[serverConfigKey{namespace, config.Name}] = copy
	return nil
}

func (s *memoryConfigStore) Delete(ctx context.Context, namespace, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.configs, serverConfigKey{namespace, name})
	return nil
}

func (s *memoryConfigStore) ListServerConfigs(ctx context.Context, namespace string, runContext map[string]any) ([]ServerConfig, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := []ServerConfig{}
	for _, config := range s.configs {
		if config.Namespace != "" && config.Namespace != namespace {
			continue
		}
		copy, err := cloneServerConfig(config)
		if err != nil {
			return nil, err
		}
		result = append(result, copy)
	}
	sortServerConfigs(result)
	return result, nil
}

// fileConfigStore reads a JSON array of ServerConfig records. Writes are atomic,
// files are private, and each listing reloads the file so edits become visible on the next run.
// Share one instance for in-process writes; use a transactional database store across processes.
type fileConfigStore struct {
	path string
	mu   sync.Mutex
}

func newFileConfigStore(path string) (*fileConfigStore, error) {
	if path == "" {
		return nil, errors.New("MCP config file path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	store := &fileConfigStore{path: absolute}
	_, err = store.read()
	return store, err
}

func (s *fileConfigStore) read() ([]ServerConfig, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return []ServerConfig{}, nil
	}
	if err != nil {
		return nil, err
	}
	var configs []ServerConfig
	if err := json.Unmarshal(data, &configs); err != nil {
		return nil, errors.New("invalid MCP server configuration JSON")
	}

	// Only developer-owned globals fail the file. User records are validated by the client,
	// which isolates a bad one to its own connector; failing here would also block the
	// writes that repair it and break every other namespace.
	seen := map[string]bool{}
	for _, config := range configs {
		if config.Namespace != "" {
			continue
		}
		if err := validateServerConfig(config); err != nil {
			return nil, err
		}
		if seen[config.Name] {
			return nil, fmt.Errorf("duplicate MCP server %q in one scope", config.Name)
		}
		seen[config.Name] = true
	}
	return configs, nil
}

func (s *fileConfigStore) ListServerConfigs(ctx context.Context, namespace string, runContext map[string]any) ([]ServerConfig, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	configs, err := s.read()
	if err != nil {
		return nil, err
	}
	result := []ServerConfig{}
	for _, config := range configs {
		if config.Namespace == "" || config.Namespace == namespace {
			result = append(result, config)
		}
	}
	sortServerConfigs(result)
	return result, nil
}

func (s *fileConfigStore) Put(ctx context.Context, namespace string, config ServerConfig) error {
	config.Namespace = namespace
	if err := validateServerConfig(config); err != nil {
		return err
	}
	return s.update(ctx, namespace, config.Name, &config)
}

func (s *fileConfigStore) Delete(ctx context.Context, namespace, name string) error {
	return s.update(ctx, namespace, name, nil)
}

// update replaces only one scope/name pair and never exposes a partially written file.
func (s *fileConfigStore) update(ctx context.Context, namespace, name string, replacement *ServerConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	configs, err := s.read()
	if err != nil {
		return err
	}
	result := make([]ServerConfig, 0, len(configs)+1)
	for _, config := range configs {
		if config.Namespace != namespace || config.Name != name {
			result = append(result, config)
		}
	}
	if replacement != nil {
		result = append(result, *replacement)
	}
	sortServerConfigs(result)
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return errors.New("MCP server config must be JSON serializable")
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(s.path), ".mcp-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Rename(file.Name(), s.path)
}

func sortServerConfigs(configs []ServerConfig) {
	sort.Slice(configs, func(i, j int) bool {
		if configs[i].Namespace != configs[j].Namespace {
			return configs[i].Namespace < configs[j].Namespace
		}
		return configs[i].Name < configs[j].Name
	})
}
