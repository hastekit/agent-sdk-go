package mcpclient

import (
	"context"
	"fmt"
)

// InlineStore serves server definitions written in code, the same for every
// namespace, and never accepts writes. As a Client's built-in store, its
// servers are global; as its user store, every user's own:
//
//	builtins := mcpclient.NewInlineStore(mcpclient.ServerConfig{Name: "docs", Endpoint: "https://docs.example/mcp"})
//	client := mcpclient.NewClient(builtins, mcpclient.WithUserStore(userStore))
//	handler := mcpclient.NewHandler(userStore, mcpclient.WithBuiltInStore(builtins))
type InlineStore struct {
	configs []ServerConfig
	err     error
}

// NewInlineStore validates and copies configs, so later changes to the
// caller's maps and slices do not apply. A definition must leave Namespace
// empty: the Client sets it from the store it reads. An invalid one is reported when the store is listed, like an invalid
// database record, so the store can be built in a single expression.
func NewInlineStore(configs ...ServerConfig) *InlineStore {
	store := &InlineStore{}
	for _, config := range configs {
		copy, err := cloneServerConfig(config)
		if err == nil && copy.Namespace != "" {
			err = fmt.Errorf("inline MCP server %q must leave its namespace empty", copy.Name)
		}
		if err == nil {
			err = validateServerConfig(copy)
		}
		if err != nil {
			store.err = err
			return store
		}
		store.configs = append(store.configs, copy)
	}
	sortServerConfigs(store.configs)
	return store
}

// ListServerConfigs returns a copy of every definition, whatever the namespace.
func (s *InlineStore) ListServerConfigs(ctx context.Context, _ string, _ map[string]any) ([]ServerConfig, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.err != nil {
		return nil, s.err
	}
	configs := make([]ServerConfig, 0, len(s.configs))
	for _, config := range s.configs {
		copy, err := cloneServerConfig(config)
		if err != nil {
			return nil, err
		}
		configs = append(configs, copy)
	}
	return configs, nil
}

var _ Store = (*InlineStore)(nil)
