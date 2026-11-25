package mcpclient

import (
	"context"
	"errors"
)

// Store lists one namespace's server definitions in one consistent snapshot.
// It is everything a Client needs, so built-in servers, filters and custom
// backends implement only this. It mirrors skills.Store.
//
// A store does not know whether its servers are global or a user's: that is
// the Client's to say, by which of its two stores it reads (see NewClient). A
// namespace is an opaque key, the empty one included, under which the Client
// reads built-in servers. A database adapter selects the records whose
// namespace matches; a store with no notion of namespaces, such as
// InlineStore, serves the same definitions for every one.
type Store interface {
	ListServerConfigs(ctx context.Context, namespace string, runContext map[string]any) ([]ServerConfig, error)
}

// MutableStore is a Store that also saves definitions, as skills.MutableStore
// is for skills. Custom database stores can implement it to enable the HTTP
// write routes.
type MutableStore interface {
	Store
	Put(ctx context.Context, namespace string, config ServerConfig) error
	Delete(ctx context.Context, namespace, name string) error
}

// validatedStore validates every write before it reaches its source, so a
// definition passes the same checks whatever persistence backs it.
type validatedStore struct {
	source Store
}

// NewStore adapts a custom source, such as a database-backed config store,
// validating every write before it reaches it. A source that is not a
// MutableStore is read-only: writes return ErrReadOnlyConfig. For server
// definitions written in code, use NewInlineStore.
func NewStore(source Store) MutableStore {
	return &validatedStore{source: source}
}

// NewMemoryStore creates process-local configuration persistence.
func NewMemoryStore() MutableStore {
	return NewStore(newMemoryConfigStore())
}

// NewFileStore keeps a JSON array of server configs, each under its
// namespace. Records with no namespace are what a Client reads as built-ins
// when the store is its built-in store.
func NewFileStore(path string) (MutableStore, error) {
	source, err := newFileConfigStore(path)
	if err != nil {
		return nil, err
	}
	return NewStore(source), nil
}

// ListServerConfigs returns a fresh snapshot of the source, ordered by scope and name.
func (s *validatedStore) ListServerConfigs(ctx context.Context, namespace string, runContext map[string]any) ([]ServerConfig, error) {
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
func (s *validatedStore) writable() (MutableStore, error) {
	source, ok := s.source.(MutableStore)
	if !ok {
		return nil, ErrReadOnlyConfig
	}
	return source, nil
}

// Put persists a definition in the caller-authorized namespace.
func (s *validatedStore) Put(ctx context.Context, namespace string, config ServerConfig) error {
	// Enforce the same boundary when the backing store is supplied by the application.
	config.Namespace = namespace
	if err := validateServerConfig(config); err != nil {
		return err
	}
	// A store keeps definitions that serialize; a credentials provider is code's alone.
	if config.CredentialsProvider != nil {
		return errors.New("an MCP server's credentials provider is for definitions written in code (NewInlineStore); a store cannot keep one")
	}

	source, err := s.writable()
	if err != nil {
		return err
	}
	return source.Put(ctx, namespace, config)
}

// Delete removes a persisted definition.
func (s *validatedStore) Delete(ctx context.Context, namespace, name string) error {
	source, err := s.writable()
	if err != nil {
		return err
	}
	return source.Delete(ctx, namespace, name)
}
