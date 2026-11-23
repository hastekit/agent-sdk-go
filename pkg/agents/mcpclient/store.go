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

// Store validates every write before it reaches its source, so a definition
// passes the same checks whatever persistence backs it. For server definitions
// written in code, use NewInlineStore.
type Store struct {
	source MCPServerConfigStore
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

// ListServerConfigs returns a fresh snapshot of the source, ordered by scope and name.
func (s *Store) ListServerConfigs(ctx context.Context, namespace string, runContext map[string]any) ([]ServerConfig, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.source == nil {
		return nil, nil
	}
	configs, err := s.source.ListServerConfigs(ctx, namespace, runContext)
	if err != nil {
		return nil, err
	}
	sortServerConfigs(configs)
	return configs, nil
}

var ErrReadOnlyConfig = errors.New("MCP configuration is read-only")

// writable rejects sources that do not implement persistence.
func (s *Store) writable() (MutableMCPServerConfigStore, error) {
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

	source, err := s.writable()
	if err != nil {
		return err
	}
	return source.Put(ctx, namespace, config)
}

// Delete removes a persisted definition.
func (s *Store) Delete(ctx context.Context, namespace, name string) error {
	source, err := s.writable()
	if err != nil {
		return err
	}
	return source.Delete(ctx, namespace, name)
}
