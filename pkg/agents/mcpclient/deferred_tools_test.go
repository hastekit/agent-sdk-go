package mcpclient

import (
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Deferral selects initial availability without changing which tools the client exposes.
func TestDeferredToolsSelection(t *testing.T) {
	tests := []struct {
		name     string
		filter   *ToolFilter
		deferred []string
	}{
		{name: "not configured"},
		{name: "empty include", filter: &ToolFilter{}, deferred: []string{"search", "book", "cancel"}},
		{name: "include", filter: &ToolFilter{Include: []string{"book"}}, deferred: []string{"book"}},
		{name: "exclude only", filter: &ToolFilter{Exclude: []string{"search"}}, deferred: []string{"book", "cancel"}},
		{name: "exclude wins", filter: &ToolFilter{Include: []string{"search", "book"}, Exclude: []string{"search"}}, deferred: []string{"book"}},
		{name: "wildcard include", filter: &ToolFilter{Include: []string{"*"}, Exclude: []string{"search"}}, deferred: []string{"book", "cancel"}},
		{name: "wildcard exclude", filter: &ToolFilter{Include: []string{"book"}, Exclude: []string{"*"}}},
		{name: "unknown include", filter: &ToolFilter{Include: []string{"missing"}}},
		{name: "unknown exclude", filter: &ToolFilter{Exclude: []string{"missing"}}, deferred: []string{"search", "book", "cancel"}},
		{name: "exact names", filter: &ToolFilter{Include: []string{"Search", "xyz__book", "can*"}}},
		{name: "unprefixed exclusions", filter: &ToolFilter{Exclude: []string{"xyz__search"}}, deferred: []string{"search", "book", "cancel"}},
	}

	// Prefixing must not change the selection made against original server names.
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, prefix := range []string{"", "xyz__"} {
				options := []McpServerOption{WithToolPrefix(prefix), WithApprovalRequiredTools("search")}
				if tt.filter != nil {
					options = append(options, WithDeferredTools(*tt.filter))
				}
				client, err := NewClient(t.Context(), "deferred", "", options...)
				require.NoError(t, err)
				tools := client.buildLazyTools([]*mcp.Tool{{Name: "search"}, {Name: "book"}, {Name: "cancel"}}, nil, serverConn{})

				// All tools remain exposed; only their discovery requirement changes.
				require.Len(t, tools, 3)
				assert.Equal(t, []string{prefix + "search", prefix + "book", prefix + "cancel"}, exposedNames(tools))
				var deferred []string
				for _, tool := range tools {
					if descriptor := tool.GetToolDescriptor(); descriptor.Deferred {
						deferred = append(deferred, descriptor.Name)
					}
				}
				assert.Equal(t, tt.deferred, deferred)
				assert.True(t, tools[0].GetToolDescriptor().RequiresApproval)
			}
		})
	}
}

// Excluding a tool from deferral must not restore a tool hidden by the visibility filter.
func TestDeferredToolsRespectsToolFilter(t *testing.T) {
	client, err := NewClient(t.Context(), "filtered", "",
		WithToolFilter(ToolFilter{Exclude: []string{"cancel"}}),
		WithDeferredTools(ToolFilter{Exclude: []string{"search", "cancel"}}),
	)
	require.NoError(t, err)
	tools := client.buildLazyTools([]*mcp.Tool{{Name: "search"}, {Name: "book"}, {Name: "cancel"}}, nil, serverConn{})

	// Search stays direct, book requires discovery, and cancel is not exposed at all.
	require.Equal(t, []string{"search", "book"}, exposedNames(tools))
	assert.False(t, tools[0].GetToolDescriptor().Deferred)
	assert.True(t, tools[1].GetToolDescriptor().Deferred)
}

// Cached server schemas are shared while each client applies its own deferral selection.
func TestDeferredToolsWithAndWithoutCache(t *testing.T) {
	for _, cached := range []bool{false, true} {
		name := "uncached"
		if cached {
			name = "cached"
		}
		t.Run(name, func(t *testing.T) {
			url, lists := cacheableServer(t, time.Minute, cacheScopePublic)
			var cache SchemaCache
			if cached {
				cache = newMemCache()
			}

			// Read through two clients whose only difference is direct tool availability.
			deferred := cachingClient(t, url, cache, WithDeferredTools(ToolFilter{}))
			direct := cachingClient(t, url, cache, WithDeferredTools(ToolFilter{Exclude: []string{"echo"}}))
			for range 2 {
				for _, client := range []*MCPClient{deferred, direct} {
					tools, err := client.ListTools(t.Context(), nil)
					require.NoError(t, err)
					require.Len(t, tools, 1)
					assert.Equal(t, "echo", tools[0].GetToolDescriptor().Name)
					assert.Equal(t, client == deferred, tools[0].GetToolDescriptor().Deferred)
				}
			}

			// Deferral options never split the shared schema cache or mutate its contents.
			if cached {
				assert.Equal(t, 1, lists())
			} else {
				assert.Equal(t, 4, lists())
			}
		})
	}
}
