package mcpclient

import (
	"context"
	"slices"
)

// FilterOption configures Filter.
type FilterOption func(*filteredStore)

// KeepUserServers keeps every namespace-owned server that Filter's map does not
// name, unchanged, for agents that should see whatever servers users add. Named
// user servers are still narrowed to their tools.
func KeepUserServers() FilterOption {
	return func(s *filteredStore) { s.keepUsers = true }
}

// filteredStore lists only the chosen servers of another store, each narrowed
// to its chosen tools.
type filteredStore struct {
	store     MCPServerConfigStore
	servers   map[string]ToolFilter
	keepUsers bool
}

// Filter returns a store that lists only the servers named in servers, each
// narrowed by its ToolFilter, so an agent can take part of a shared catalog
// instead of all of it. Names match global and namespace-owned servers alike:
//
//	shared, err := mcpclient.NewFileStore("mcp.json")
//	client := mcpclient.NewClient(mcpclient.Filter(shared, map[string]mcpclient.ToolFilter{
//		"docs":   {},                                     // a global, every tool it exposes
//		"github": {Include: []string{"get_issue", "search_issues"}},
//		"gmail":  {Include: []string{"search"}},          // each user's own gmail server
//	}))
//
// A namespace's servers the map does not name are dropped, since users choose
// their own names; pass KeepUserServers to keep them. When a global and a
// user's server share a kept name, the global wins as usual.
//
// Tool names are the server's original names, before any ToolPrefix. A tool
// filter narrows the server's own ServerConfig.ToolFilter and can never restore
// a tool it excludes; an empty ToolFilter keeps the server's tools as they are.
// Per-run MCPSelection.Tools narrows the result further.
//
// The filter applies to every listing, including the one CallTool makes, so a
// server or tool left out is neither listed nor callable through the client. A
// name the store does not list is ignored, since a catalog can change at run
// time. An empty map lists nothing, or only users' servers with KeepUserServers.
//
// A global left out is not the agent's at all, so it does not shadow a kept
// user server with the same name. Stored OAuth grants are bound to the global
// definition (see OAuthCredentialKey), so such a user server never receives them.
//
// The result is read-only. Give NewHandler the unfiltered store, so users manage
// their servers there and every global name stays reserved.
func Filter(store MCPServerConfigStore, servers map[string]ToolFilter, options ...FilterOption) MCPServerConfigStore {
	if store == nil {
		return nil // reported by the client like any nil store
	}
	// Copy so later changes to the caller's map or slices cannot widen the filter.
	filtered := &filteredStore{store: store, servers: make(map[string]ToolFilter, len(servers))}
	for name, tools := range servers {
		filtered.servers[name] = ToolFilter{Include: slices.Clone(tools.Include), Exclude: slices.Clone(tools.Exclude)}
	}
	for _, option := range options {
		if option != nil {
			option(filtered)
		}
	}
	return filtered
}

func (s *filteredStore) ListServerConfigs(ctx context.Context, namespace string, runContext map[string]any) ([]ServerConfig, error) {
	listed, err := s.store.ListServerConfigs(ctx, namespace, runContext)
	if err != nil {
		return nil, err
	}
	configs := make([]ServerConfig, 0, len(listed))
	for _, config := range listed {
		tools, named := s.servers[config.Name]
		switch {
		case named:
			config.ToolFilter = config.ToolFilter.narrow(tools)
		case config.Namespace == "" || !s.keepUsers:
			continue
		}
		configs = append(configs, config)
	}
	return configs, nil
}

// narrow returns a filter that allows only the tools both f and g allow.
func (f ToolFilter) narrow(g ToolFilter) ToolFilter {
	result := ToolFilter{Exclude: append(slices.Clone(f.Exclude), g.Exclude...)}
	switch {
	case len(g.Include) == 0:
		result.Include = slices.Clone(f.Include)
	case len(f.Include) == 0:
		result.Include = slices.Clone(g.Include)
	default:
		// Keep g's list and exclude the names f does not include. An empty
		// intersection then allows nothing, rather than reading as "all tools".
		result.Include = slices.Clone(g.Include)
		for _, name := range g.Include {
			if !slices.Contains(f.Include, name) {
				result.Exclude = append(result.Exclude, name)
			}
		}
	}
	return result
}
