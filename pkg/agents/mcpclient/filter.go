package mcpclient

import (
	"context"
	"slices"
)

// filteredStore lists only the chosen servers of another store, each narrowed
// to its chosen tools.
type filteredStore struct {
	store   Store
	servers map[string]ToolFilter
}

// Filter returns a store that lists only the servers named in servers, each
// narrowed by its ToolFilter, in every namespace, so an agent can take part of
// a store instead of all of it. Filter either of a Client's stores, or both:
//
//	client := mcpclient.NewClient(
//		mcpclient.Filter(builtIns, map[string]mcpclient.ToolFilter{
//			"docs":   {},                                     // every tool it exposes
//			"github": {Include: []string{"get_issue", "search_issues"}},
//		}),
//		mcpclient.WithUserStore(mcpclient.Filter(users, map[string]mcpclient.ToolFilter{
//			"gmail": {Include: []string{"search"}},           // each user's own gmail server
//		})),
//	)
//
// Tool names are the server's original names, before any ToolPrefix. A tool
// filter narrows the server's own ServerConfig.ToolFilter and can never restore
// a tool it excludes; an empty ToolFilter keeps the server's tools as they are.
// Per-run MCPSelection.Tools narrows a user's server further.
//
// The filter applies to every listing, including the one CallTool makes, so a
// server or tool left out is neither listed nor callable through the client. A
// name the store does not list is ignored, since a catalog can change at run
// time. An empty map lists nothing.
//
// The result is read-only. Give NewHandler the unfiltered stores.
func Filter(store Store, servers map[string]ToolFilter) Store {
	if store == nil {
		return nil // reported by the client like any nil store
	}
	// Copy so later changes to the caller's map or slices cannot widen the filter.
	filtered := &filteredStore{store: store, servers: make(map[string]ToolFilter, len(servers))}
	for name, tools := range servers {
		filtered.servers[name] = ToolFilter{Include: slices.Clone(tools.Include), Exclude: slices.Clone(tools.Exclude)}
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
		if !named {
			continue
		}
		config.ToolFilter = config.ToolFilter.narrow(tools)
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
