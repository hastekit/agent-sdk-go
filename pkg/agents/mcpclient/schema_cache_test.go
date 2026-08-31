package mcpclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cacheableServer answers tools/list with the directives a 2026-07-28 server
// sends (SEP-2549), and counts how often it was actually asked.
func cacheableServer(t *testing.T, ttl time.Duration, scope string) (url string, lists func() int) {
	t.Helper()
	calls := 0

	server := mcp.NewServer(&mcp.Implementation{Name: "cacheable", Version: "0.1.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
		})
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			list, ok := res.(*mcp.ListToolsResult)
			if !ok {
				return res, err
			}
			calls++
			list.TTLMs = int(ttl / time.Millisecond)
			list.CacheScope = scope
			return list, err
		}
	})

	hs := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server }, nil))
	t.Cleanup(func() { hs.CloseClientConnections(); hs.Close() })

	return hs.URL, func() int { return calls }
}

func cachingClient(t *testing.T, url string, cache SchemaCache, opts ...McpServerOption) *MCPClient {
	t.Helper()
	base := []McpServerOption{WithTransport(TransportStreamableHTTP), WithSchemaCache(cache)}
	client, err := NewClient(context.Background(), "cached", url, append(base, opts...)...)
	require.NoError(t, err)
	return client
}

// A listing the server calls public is the same for everyone, so it is stored
// where every run finds it — one fetch serves the second user.
func TestPublicListingIsSharedAcrossPrincipals(t *testing.T) {
	url, lists := cacheableServer(t, time.Minute, cacheScopePublic)
	cache := newMemCache()

	client := cachingClient(t, url, cache, WithCredentialProvider(
		keyed(&stubProvider{tokens: map[string]string{"cached/ada": "a", "cached/grace": "g"}})))

	ctx := context.Background()
	_, err := client.ListTools(ctx, map[string]any{"user": "ada"})
	require.NoError(t, err)
	_, err = client.ListTools(ctx, map[string]any{"user": "grace"})
	require.NoError(t, err)

	assert.Equal(t, 1, lists(), "the second user reads the first user's entry")
	require.Len(t, cache.keys(), 1)
	assert.NotContains(t, cache.keys()[0], "ada", "a shared entry names no requester")
	assert.NotContains(t, cache.keys()[0], "grace")
}

// A listing the server calls private varies by who asked, so each principal
// gets an entry of its own and neither can read the other's.
func TestPrivateListingIsKeyedPerPrincipal(t *testing.T) {
	url, lists := cacheableServer(t, time.Minute, cacheScopePrivate)
	cache := newMemCache()

	client := cachingClient(t, url, cache, WithCredentialProvider(
		keyed(&stubProvider{tokens: map[string]string{"cached/ada": "a", "cached/grace": "g"}})))

	ctx := context.Background()
	for _, user := range []string{"ada", "grace", "ada"} {
		_, err := client.ListTools(ctx, map[string]any{"user": user})
		require.NoError(t, err)
	}

	assert.Equal(t, 2, lists(), "two principals, two fetches — the third call is a hit")
	keys := cache.keys()
	require.Len(t, keys, 2)
	assert.True(t, strings.HasSuffix(keys[0], "|ada") || strings.HasSuffix(keys[0], "|grace"))
}

// A server too old to have been asked said nothing, and silence is not the
// promise that an explicit "public" is.
func TestAServerThatSaysNothingIsTreatedAsPrivate(t *testing.T) {
	url, lists := cacheableServer(t, 0, "") // pre-2026-07-28: no directives at all
	cache := newMemCache()

	client := cachingClient(t, url, cache,
		WithCacheTTL(time.Minute),
		WithCredentialProvider(keyed(&stubProvider{
			tokens: map[string]string{"cached/ada": "a", "cached/grace": "g"}})))

	ctx := context.Background()
	_, err := client.ListTools(ctx, map[string]any{"user": "ada"})
	require.NoError(t, err)
	_, err = client.ListTools(ctx, map[string]any{"user": "grace"})
	require.NoError(t, err)

	assert.Equal(t, 2, lists(), "one user's listing is not served to another")
	assert.Len(t, cache.keys(), 2)
}

func TestServerTTLIsStoredAndHonoured(t *testing.T) {
	url, lists := cacheableServer(t, 40*time.Millisecond, cacheScopePublic)
	cache := newMemCache()
	client := cachingClient(t, url, cache)

	ctx := context.Background()
	_, err := client.ListTools(ctx, nil)
	require.NoError(t, err)

	key := cache.keys()[0]
	assert.Equal(t, 40*time.Millisecond, cache.ttl(key), "the server's ttlMs reaches the store")

	_, err = client.ListTools(ctx, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, lists(), "still fresh")

	// Expire it on the entry rather than by sleeping.
	entry, ok := cache.Get(ctx, key)
	require.True(t, ok)
	entry.ExpiresAt = time.Now().Add(-time.Second)

	_, err = client.ListTools(ctx, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, lists(), "a stale entry is refetched, not served")
}

// WithCacheTTL bounds what the server asked for, the way a proxy's own max-age
// bounds an upstream's.
func TestConfiguredTTLCapsTheServers(t *testing.T) {
	url, _ := cacheableServer(t, time.Hour, cacheScopePublic)
	cache := newMemCache()
	client := cachingClient(t, url, cache, WithCacheTTL(time.Minute))

	_, err := client.ListTools(context.Background(), nil)
	require.NoError(t, err)

	assert.Equal(t, time.Minute, cache.ttl(cache.keys()[0]))
}

// Invalidation cannot know which key the server's answer landed under.
func TestInvalidateDropsBothKeys(t *testing.T) {
	url, lists := cacheableServer(t, time.Minute, cacheScopePublic)
	cache := newMemCache()
	client := cachingClient(t, url, cache, WithCredentialProvider(
		keyed(&stubProvider{tokens: map[string]string{"cached/ada": "a"}})))

	ctx := context.Background()
	run := map[string]any{"user": "ada"}
	_, err := client.ListTools(ctx, run)
	require.NoError(t, err)
	require.Equal(t, 1, lists())

	client.InvalidateToolCache(ctx, run)
	assert.Empty(t, cache.keys())

	_, err = client.ListTools(ctx, run)
	require.NoError(t, err)
	assert.Equal(t, 2, lists())
}

// Without a credential provider the requester is still told apart by the
// credentials it presents, which is how it worked before principals existed.
func TestPrivateListingFallsBackToHeadersWithoutAPrincipal(t *testing.T) {
	url, _ := cacheableServer(t, time.Minute, cacheScopePrivate)
	cache := newMemCache()

	ada := cachingClient(t, url, cache, WithHeaders(map[string]string{"X-User": "ada"}))
	grace := cachingClient(t, url, cache, WithHeaders(map[string]string{"X-User": "grace"}))

	ctx := context.Background()
	_, err := ada.ListTools(ctx, nil)
	require.NoError(t, err)
	_, err = grace.ListTools(ctx, nil)
	require.NoError(t, err)

	assert.Len(t, cache.keys(), 2)
}

// What is cached is the server's listing, not any client's view of it. Two
// clients that admit different tools — or expose them under different names —
// read the same entry and narrow it themselves on the way out.
func TestFilterAndPrefixShareOneCacheEntry(t *testing.T) {
	url, lists := cacheableServer(t, time.Minute, cacheScopePublic)
	cache := newMemCache()

	ctx := context.Background()
	unfiltered := cachingClient(t, url, cache)
	filtered := cachingClient(t, url, cache, WithToolFilter("nothing-matches"))
	prefixed := cachingClient(t, url, cache, WithToolPrefix("xyz__"))

	all, err := unfiltered.ListTools(ctx, nil)
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, "echo", all[0].GetToolDescriptor().ToolUnion.OfFunction.Name)

	none, err := filtered.ListTools(ctx, nil)
	require.NoError(t, err)
	assert.Empty(t, none, "the filter still applies — it just applies after the cache")

	renamed, err := prefixed.ListTools(ctx, nil)
	require.NoError(t, err)
	require.Len(t, renamed, 1)
	assert.Equal(t, "xyz__echo", renamed[0].GetToolDescriptor().ToolUnion.OfFunction.Name)

	assert.Equal(t, 1, lists(), "one listing served all three")
	assert.Equal(t, []string{"mcp:schema:cached"}, cache.keys())
}

// The key is the connector's name, so a server that moves keeps its entry
// rather than silently starting cold.
func TestCacheKeyFollowsTheConnectorName(t *testing.T) {
	url, _ := cacheableServer(t, time.Minute, cacheScopePublic)
	cache := newMemCache()

	client := cachingClient(t, url, cache)
	_, err := client.ListTools(context.Background(), nil)
	require.NoError(t, err)

	assert.Equal(t, []string{"mcp:schema:cached"}, cache.keys())
}
