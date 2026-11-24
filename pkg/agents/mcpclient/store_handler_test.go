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

	// The client sets an inline definition's namespace; one that names its own, or
	// an invalid value, fails the listing.
	for _, bad := range []ServerConfig{
		{Name: "docs", Namespace: "alice", Endpoint: "https://inline.example/mcp"},
		{Name: "docs", Transport: "carrier-pigeon"},
	} {
		_, err = NewInlineStore(bad).ListServerConfigs(t.Context(), "alice", nil)
		require.Error(t, err)
	}

	// As the built-in store, its server shadows a user's with the same name.
	persisted := NewMemoryStore()
	require.NoError(t, persisted.Put(t.Context(), "alice", ServerConfig{Name: "docs", Endpoint: "https://user.example/mcp"}))
	resolved, err := NewClient(inline, WithUserStore(persisted)).listServerConfigs(t.Context(), "alice", nil)
	require.NoError(t, err)
	require.Equal(t, "https://inline.example/mcp", resolved.configs["docs"].Endpoint)
	require.Equal(t, "", resolved.configs["docs"].Namespace)

	// As the user store, its servers are each user's own.
	resolved, err = NewClient(nil, WithUserStore(inline)).listServerConfigs(t.Context(), "alice", nil)
	require.NoError(t, err)
	require.Equal(t, "alice", resolved.configs["docs"].Namespace)
}

type contextConfigStore struct {
	namespace  string
	runContext map[string]any
}

func (s *contextConfigStore) ListServerConfigs(_ context.Context, namespace string, runContext map[string]any) ([]ServerConfig, error) {
	s.namespace, s.runContext = namespace, runContext
	return nil, nil
}

func TestClientReadsEachStoreUnderItsNamespace(t *testing.T) {
	// The built-in store is read under the empty namespace and the user store under
	// the authoritative one; both receive the same run context.
	builtIn, user := &contextConfigStore{}, &contextConfigStore{}
	client := NewClient(builtIn, WithUserStore(user))
	_, _, err := client.ListTools(t.Context(), "alice", map[string]any{"thread_id": "thread"})
	require.NoError(t, err)
	require.Equal(t, "", builtIn.namespace)
	require.Equal(t, "alice", user.namespace)
	for _, source := range []*contextConfigStore{builtIn, user} {
		require.Equal(t, "thread", source.runContext["thread_id"])
	}

	// A built-in defined twice fails clearly instead of choosing one by accident.
	configs := []ServerConfig{{Name: "duplicate", Endpoint: "https://example.com/mcp"}}
	client = NewClient(NewInlineStore(append(configs, configs...)...))
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
	handler := NewHandler(store, WithBuiltInStore(builtins), WithNamespaceResolver(func(r *http.Request) (string, error) { return r.Header.Get("Subject"), nil }))
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
// per-user server the application provisions: the user's own, but read-only.
type provisionedStore struct{}

func (provisionedStore) ListServerConfigs(_ context.Context, namespace string, _ map[string]any) ([]ServerConfig, error) {
	return []ServerConfig{{Name: "provisioned", Namespace: namespace, Endpoint: "https://provisioned.example/mcp"}}, nil
}

func TestHandlerNeverWritesReadOnlyStores(t *testing.T) {
	builtins := NewInlineStore(ServerConfig{Name: "docs", Endpoint: "https://docs.example/mcp"})
	serve := func(user MutableStore) func(method, path, body string) *httptest.ResponseRecorder {
		handler := NewHandler(user, WithBuiltInStore(builtins), WithNamespaceResolver(func(*http.Request) (string, error) { return "alice", nil }))
		return func(method, path, body string) *httptest.ResponseRecorder {
			result := httptest.NewRecorder()
			handler.ServeHTTP(result, httptest.NewRequest(method, path, strings.NewReader(body)))
			return result
		}
	}
	listed := func(request func(method, path, body string) *httptest.ResponseRecorder) map[string]bool {
		response := request("GET", "/", "")
		require.Equal(t, http.StatusOK, response.Code)
		var summaries []ConnectorInfo
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &summaries))
		readOnly := map[string]bool{}
		for _, summary := range summaries {
			readOnly[summary.Name] = summary.ReadOnly
		}
		return readOnly
	}

	// Built-ins are listed read-only; the user store's records are editable.
	store := NewMemoryStore()
	require.NoError(t, store.Put(t.Context(), "alice", ServerConfig{Name: "mine", Endpoint: "https://mine.example/mcp"}))
	request := serve(store)
	require.Equal(t, map[string]bool{"docs": true, "mine": false}, listed(request))
	require.Equal(t, http.StatusForbidden, request("PUT", "/docs", `{"endpoint":"https://evil.example/mcp"}`).Code)
	require.Equal(t, http.StatusForbidden, request("DELETE", "/docs", "").Code)
	records, err := store.ListServerConfigs(t.Context(), "alice", nil)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, http.StatusNoContent, request("PUT", "/mine", `{"endpoint":"https://mine.example/v2"}`).Code)
	require.Equal(t, http.StatusNoContent, request("DELETE", "/mine", "").Code)

	// A user store whose source cannot persist lists the user's servers read-only and refuses writes.
	request = serve(NewStore(provisionedStore{}))
	require.Equal(t, map[string]bool{"docs": true, "provisioned": true}, listed(request))
	for _, name := range []string{"provisioned", "new"} {
		require.Equal(t, http.StatusForbidden, request("PUT", "/"+name, `{"endpoint":"https://evil.example/mcp"}`).Code, name)
		require.Equal(t, http.StatusForbidden, request("DELETE", "/"+name, "").Code, name)
	}
}
