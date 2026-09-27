package mcpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStoreInlineCompositionIsImmutable(t *testing.T) {
	// Inline values replace matching records in the copy, while the base remains unchanged.
	base := NewMemoryStore()
	require.NoError(t, base.Put(t.Context(), "", ServerConfig{Name: "docs", Endpoint: "https://original.example/mcp"}))
	configs := []ServerConfig{{Name: "docs", Endpoint: "https://inline.example/mcp", Headers: map[string]string{"key": "original"}}}
	overlay := base.WithMCPServerConfig(configs)
	configs[0].Headers["key"] = "mutated"
	listed, err := overlay.ListServerConfigs(t.Context(), "alice", nil)
	require.NoError(t, err)
	require.Equal(t, "original", listed[0].Headers["key"])
	listed[0].Headers["key"] = "changed again"
	listed, err = overlay.ListServerConfigs(t.Context(), "alice", nil)
	require.NoError(t, err)
	require.Equal(t, "original", listed[0].Headers["key"])
	original, err := base.ListServerConfigs(t.Context(), "alice", nil)
	require.NoError(t, err)
	require.Equal(t, "https://original.example/mcp", original[0].Endpoint)

	// Derived views see new persisted records but cannot overwrite their own inline definitions.
	require.NoError(t, overlay.Put(t.Context(), "alice", ServerConfig{Name: "notes", Endpoint: "https://notes.example/mcp"}))
	original, err = base.ListServerConfigs(t.Context(), "alice", nil)
	require.NoError(t, err)
	require.Len(t, original, 2)
	require.ErrorIs(t, overlay.Delete(t.Context(), "", "docs"), ErrReadOnlyConfig)
}

type contextConfigStore struct {
	namespace  string
	runContext map[string]any
}

func (s *contextConfigStore) ListServerConfigs(_ context.Context, namespace string, runContext map[string]any) ([]ServerConfig, error) {
	s.namespace, s.runContext = namespace, runContext
	return nil, nil
}

func TestClientMultipleStoresAndRunContext(t *testing.T) {
	// All sources receive authoritative identity and the same run context.
	first, second := &contextConfigStore{}, &contextConfigStore{}
	client := NewClient(first, second)
	_, _, err := client.ListTools(t.Context(), "alice", map[string]any{"thread_id": "thread"})
	require.NoError(t, err)
	for _, source := range []*contextConfigStore{first, second} {
		require.Equal(t, "alice", source.namespace)
		require.Equal(t, "thread", source.runContext["thread_id"])
	}

	// Duplicate source definitions fail clearly instead of choosing a store by accident.
	configs := []ServerConfig{{Name: "duplicate", Endpoint: "https://example.com/mcp"}}
	client = NewClient(NewMemoryStore().WithMCPServerConfig(configs), NewMemoryStore().WithMCPServerConfig(configs))
	_, _, err = client.ListTools(t.Context(), "alice", nil)
	require.ErrorContains(t, err, "duplicate")
}

func TestHandlerOwnsNamespaceAndProtectsDeveloperConfigs(t *testing.T) {
	// Use real file persistence so HTTP updates can be verified across store instances.
	path := t.TempDir() + "/servers.json"
	base, err := NewFileStore(path)
	require.NoError(t, err)
	store := base.WithMCPServerConfig([]ServerConfig{{Name: "global", Endpoint: "https://global.example/mcp", Headers: map[string]string{"Authorization": "secret-header"}}})
	require.NoError(t, store.Put(t.Context(), "bob", ServerConfig{Name: "private", Endpoint: "https://bob.example/mcp"}))
	handler := NewHandler(store, WithNamespaceResolver(func(r *http.Request) (string, error) { return r.Header.Get("Subject"), nil }))
	request := func(method, path, subject, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Subject", subject)
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, req)
		return result
	}

	// Request bodies cannot choose another subject or replace a developer-owned connector.
	require.Equal(t, http.StatusForbidden, request("GET", "/", "", "").Code)
	require.Equal(t, http.StatusForbidden, request("PUT", "/global", "alice", `{"endpoint":"https://evil.example/mcp"}`).Code)
	require.Equal(t, http.StatusForbidden, request("DELETE", "/global", "alice", "").Code)
	require.Equal(t, http.StatusNoContent, request("PUT", "/mine", "alice", `{"name":"ignored","namespace":"bob","endpoint":"https://mine.example/mcp","headers":{"key":"secret-user"}}`).Code)
	require.Equal(t, http.StatusBadRequest, request("PUT", "/shell", "alice", `{"transport":"stdio","command":["sh"]}`).Code)

	// Listing includes visible summaries without configuration secrets or other users' connectors.
	response := request("GET", "/", "alice", "")
	require.Equal(t, http.StatusOK, response.Code)
	require.NotContains(t, response.Body.String(), "secret")
	var summaries []ConnectorInfo
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &summaries))
	require.Len(t, summaries, 2)
	require.True(t, summaries[0].ReadOnly)
	require.False(t, summaries[1].ReadOnly)
	reopened, err := NewFileStore(path)
	require.NoError(t, err)
	records, err := reopened.ListServerConfigs(t.Context(), "alice", nil)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, "alice", records[0].Namespace)

	// Deleting a same-named entry in another namespace never removes that user's record.
	require.Equal(t, http.StatusNoContent, request("DELETE", "/private", "alice", "").Code)
	records, err = reopened.ListServerConfigs(t.Context(), "bob", nil)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, "private", records[0].Name)
	require.Equal(t, http.StatusNoContent, request("DELETE", "/mine", "alice", "").Code)
}

func TestOAuthHandlerReadsStoreBeforeDiscovery(t *testing.T) {
	// No MCP client or tool listing is needed to start authorization.
	provider := authorizationProvider(t)
	store := NewMemoryStore()
	config := ServerConfig{Name: "mail", Endpoint: "https://mail.example/mcp", Authorization: oauthTestConfig("https://accounts.example/token")}
	config.Authorization.RedirectURL = "https://app.example/mail/callback"
	require.NoError(t, store.Put(t.Context(), "alice", config))
	handler := NewHandler(store, WithOAuth(provider), WithNamespaceResolver(func(*http.Request) (string, error) { return "alice", nil }))
	connect := func() *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", "/mail/connect", nil))
		return response
	}
	response := connect()
	require.Equal(t, http.StatusFound, response.Code)
	require.Contains(t, response.Header().Get("Location"), "client_id=client")

	// Future flows immediately use edited settings, with no provider registration or restart.
	config.Authorization.ClientID = "replacement"
	require.NoError(t, store.Put(t.Context(), "alice", config))
	response = connect()
	require.Equal(t, http.StatusFound, response.Code)
	require.Contains(t, response.Header().Get("Location"), "client_id=replacement")
	require.NoError(t, store.Delete(t.Context(), "alice", "mail"))
	require.Equal(t, http.StatusNotFound, connect().Code)
}
