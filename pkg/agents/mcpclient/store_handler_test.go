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

func TestInlineStoreCopiesAndValidatesItsConfigs(t *testing.T) {
	// Definitions are copied in and out, so no caller mutation reaches the store.
	configs := []ServerConfig{{Name: "docs", Endpoint: "https://inline.example/mcp", Headers: map[string]string{"key": "original"}}}
	inline := NewInlineStore(configs...)
	configs[0].Headers["key"] = "mutated"
	listed, err := inline.ListServerConfigs(t.Context(), "alice", nil)
	require.NoError(t, err)
	require.Equal(t, "original", listed[0].Headers["key"])
	listed[0].Headers["key"] = "changed again"
	listed, err = inline.ListServerConfigs(t.Context(), "", nil)
	require.NoError(t, err)
	require.Equal(t, "original", listed[0].Headers["key"])

	// Inline definitions are global; a namespace or invalid value fails the listing.
	for _, bad := range []ServerConfig{
		{Name: "docs", Namespace: "alice", Endpoint: "https://inline.example/mcp"},
		{Name: "docs", Transport: "carrier-pigeon"},
	} {
		_, err = NewInlineStore(bad).ListServerConfigs(t.Context(), "alice", nil)
		require.Error(t, err)
	}

	// A server defined inline and in a persisted store is a duplicate, never an override.
	persisted := NewMemoryStore()
	require.NoError(t, persisted.Put(t.Context(), "", ServerConfig{Name: "docs", Endpoint: "https://original.example/mcp"}))
	_, _, err = NewClient(persisted, inline).ListTools(t.Context(), "alice", nil)
	require.ErrorContains(t, err, "duplicate global MCP server")
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
	client = NewClient(NewInlineStore(configs...), NewInlineStore(configs...))
	_, _, err = client.ListTools(t.Context(), "alice", nil)
	require.ErrorContains(t, err, "duplicate")
}

func TestHandlerOwnsNamespaceAndProtectsDeveloperConfigs(t *testing.T) {
	// Use real file persistence so HTTP updates can be verified across store instances.
	path := t.TempDir() + "/servers.json"
	store, err := NewFileStore(path)
	require.NoError(t, err)
	builtins := NewInlineStore(ServerConfig{Name: "global", Endpoint: "https://global.example/mcp", Headers: map[string]string{"Authorization": "secret-header"}})
	require.NoError(t, store.Put(t.Context(), "bob", ServerConfig{Name: "private", Endpoint: "https://bob.example/mcp"}))
	handler := NewHandler(store, WithReadOnlyStores(builtins), WithNamespaceResolver(func(r *http.Request) (string, error) { return r.Header.Get("Subject"), nil }))
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

// provisionedStore stands in for application-managed records, such as a
// per-user server the application provisions: listed, but not the user's to edit.
type provisionedStore struct{}

func (provisionedStore) ListServerConfigs(_ context.Context, namespace string, _ map[string]any) ([]ServerConfig, error) {
	return []ServerConfig{{Name: "provisioned", Namespace: namespace, Endpoint: "https://provisioned.example/mcp"}}, nil
}

func TestHandlerNeverWritesReadOnlyStores(t *testing.T) {
	store := NewMemoryStore()
	require.NoError(t, store.Put(t.Context(), "alice", ServerConfig{Name: "mine", Endpoint: "https://mine.example/mcp"}))
	builtins := NewInlineStore(ServerConfig{Name: "docs", Endpoint: "https://docs.example/mcp"})
	handler := NewHandler(store, WithReadOnlyStores(builtins, provisionedStore{}), WithNamespaceResolver(func(*http.Request) (string, error) { return "alice", nil }))
	request := func(method, path, body string) *httptest.ResponseRecorder {
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, httptest.NewRequest(method, path, strings.NewReader(body)))
		return result
	}

	// Every store's servers are listed; only the writable store's own records are editable.
	response := request("GET", "/", "")
	require.Equal(t, http.StatusOK, response.Code)
	var summaries []ConnectorInfo
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &summaries))
	readOnly := map[string]bool{}
	for _, summary := range summaries {
		readOnly[summary.Name] = summary.ReadOnly
	}
	require.Equal(t, map[string]bool{"docs": true, "mine": false, "provisioned": true}, readOnly)

	// Writes to a read-only store's names are refused and never reach the writable store.
	for _, name := range []string{"docs", "provisioned"} {
		require.Equal(t, http.StatusForbidden, request("PUT", "/"+name, `{"endpoint":"https://evil.example/mcp"}`).Code, name)
		require.Equal(t, http.StatusForbidden, request("DELETE", "/"+name, "").Code, name)
	}
	records, err := store.ListServerConfigs(t.Context(), "alice", nil)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, http.StatusNoContent, request("PUT", "/mine", `{"endpoint":"https://mine.example/v2"}`).Code)
	require.Equal(t, http.StatusNoContent, request("DELETE", "/mine", "").Code)
}
