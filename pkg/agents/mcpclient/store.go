package mcpclient

import (
	"context"
	"errors"
)

// MutableMCPServerConfigStore adds persistence for user-owned connector settings.
// Custom database stores can implement this interface to enable the HTTP write routes.
type MutableMCPServerConfigStore interface {
	MCPServerConfigStore
	Put(ctx context.Context, namespace string, config ServerConfig) error
	Delete(ctx context.Context, namespace, name string) error
}

// Store decorates a source with immutable inline definitions.
// Copies share the backing persistence, but never share mutable inline configurations.
type Store struct {
	source MCPServerConfigStore
	inline []ServerConfig
	err    error
}

// NewStore adapts a custom source, such as a database-backed config store.
func NewStore(source MCPServerConfigStore) *Store {
	return &Store{source: source}
}

// NewMemoryStore creates process-local configuration persistence.
func NewMemoryStore() *Store {
	return NewStore(newMemoryConfigStore())
}

// NewFileStore loads a JSON array of global and namespace-owned server configs.
func NewFileStore(path string) (*Store, error) {
	source, err := newFileConfigStore(path)
	if err != nil {
		return nil, err
	}
	return NewStore(source), nil
}

// WithMCPServerConfig returns a copy with additional inline configurations.
// Inline definitions override the same scope/name in the source and cannot be edited through HTTP.
// Invalid inline values are reported on listing, just like invalid database records.
func (s *Store) WithMCPServerConfig(configs []ServerConfig) *Store {
	// Clone both existing and added definitions so caller mutations cannot change this view.
	result := &Store{source: s.source, err: s.err}
	for _, group := range [][]ServerConfig{s.inline, configs} {
		for _, config := range group {
			copy, err := cloneServerConfig(config)
			if err == nil {
				err = validateServerConfig(copy)
			}
			if err != nil {
				result.err = err
				return result
			}
			result.inline = append(result.inline, copy)
		}
	}
	return result
}

// ListServerConfigs merges a fresh source snapshot with this view's inline definitions.
func (s *Store) ListServerConfigs(ctx context.Context, namespace string, runContext map[string]any) ([]ServerConfig, error) {
	// Fail before exposing a partial catalog when inline configuration is invalid.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.err != nil {
		return nil, s.err
	}
	var configs []ServerConfig
	if s.source != nil {
		var err error
		configs, err = s.source.ListServerConfigs(ctx, namespace, runContext)
		if err != nil {
			return nil, err
		}
	}

	// Preserve duplicate source records for catalog validation; only explicit overlays replace them.
	for _, inline := range s.inline {
		if inline.Namespace != "" && inline.Namespace != namespace {
			continue
		}
		filtered := make([]ServerConfig, 0, len(configs)+1)
		for _, config := range configs {
			if config.Namespace != inline.Namespace || config.Name != inline.Name {
				filtered = append(filtered, config)
			}
		}
		copy, err := cloneServerConfig(inline)
		if err != nil {
			return nil, err
		}
		configs = append(filtered, copy)
	}
	sortServerConfigs(configs)
	return configs, nil
}

var ErrReadOnlyConfig = errors.New("MCP configuration is read-only")

// writable rejects inline edits and sources that do not implement persistence.
func (s *Store) writable(namespace, name string) (MutableMCPServerConfigStore, error) {
	for _, config := range s.inline {
		if config.Namespace == namespace && config.Name == name {
			return nil, ErrReadOnlyConfig
		}
	}
	source, ok := s.source.(MutableMCPServerConfigStore)
	if !ok {
		return nil, ErrReadOnlyConfig
	}
	return source, nil
}

// Put persists a definition in the caller-authorized namespace.
func (s *Store) Put(ctx context.Context, namespace string, config ServerConfig) error {
	// Enforce the same boundary when the backing store is supplied by the application.
	config.Namespace = namespace
	if err := validateServerConfig(config); err != nil {
		return err
	}

	source, err := s.writable(namespace, config.Name)
	if err != nil {
		return err
	}
	return source.Put(ctx, namespace, config)
}

// Delete removes a persisted definition without changing inline configurations.
func (s *Store) Delete(ctx context.Context, namespace, name string) error {
	source, err := s.writable(namespace, name)
	if err != nil {
		return err
	}
	return source.Delete(ctx, namespace, name)
}
