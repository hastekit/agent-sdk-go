package mcpclient

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolFilter(t *testing.T) {
	tests := []struct {
		name   string
		filter ToolFilter
		want   []string
	}{
		{"empty", ToolFilter{}, []string{"search", "book", "cancel"}},
		{"include", ToolFilter{Include: []string{"book", "search"}}, []string{"search", "book"}},
		{"exclude", ToolFilter{Exclude: []string{"book"}}, []string{"search", "cancel"}},
		{"exclude wins", ToolFilter{Include: []string{"search", "book"}, Exclude: []string{"book"}}, []string{"search"}},
		{"exclude all included", ToolFilter{Include: []string{"book"}, Exclude: []string{"book"}}, []string{}},
		{"unknown include", ToolFilter{Include: []string{"missing"}}, []string{}},
		{"unknown exclude", ToolFilter{Exclude: []string{"missing"}}, []string{"search", "book", "cancel"}},
		{"exact names", ToolFilter{Exclude: []string{"Search", "*", "xyz__book"}}, []string{"search", "book", "cancel"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, prefix := range []string{"", "xyz__"} {
				client, err := NewClient(context.Background(), "filtered", "",
					WithToolFilter(tt.filter), WithToolPrefix(prefix))
				require.NoError(t, err)
				tools := client.buildLazyTools([]*mcp.Tool{
					{Name: "search"}, {Name: "book"}, {Name: "cancel"},
				}, nil, serverConn{})
				want := make([]string, 0, len(tt.want))
				for _, name := range tt.want {
					want = append(want, prefix+name)
				}
				assert.Equal(t, want, exposedNames(tools), "prefix %q", prefix)
			}
		})
	}
}

func TestListToolsExcludesToolsWithAndWithoutCache(t *testing.T) {
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
			client := cachingClient(t, url, cache,
				WithToolFilter(ToolFilter{Exclude: []string{"echo"}}), WithToolPrefix("xyz__"))
			for range 2 {
				tools, err := client.ListTools(context.Background(), nil)
				require.NoError(t, err)
				assert.Empty(t, tools)
			}

			unfiltered := cachingClient(t, url, cache)
			tools, err := unfiltered.ListTools(context.Background(), nil)
			require.NoError(t, err)
			assert.Equal(t, []string{"echo"}, exposedNames(tools), "exclusions must not remove cached schemas")
			if cached {
				assert.Equal(t, 1, lists())
			} else {
				assert.Equal(t, 3, lists())
			}
		})
	}
}
