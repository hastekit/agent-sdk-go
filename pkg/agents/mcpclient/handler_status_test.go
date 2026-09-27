package mcpclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

func TestHandlerReportsOAuthConnectionAndForgetsItOnDelete(t *testing.T) {
	store := NewMemoryStore()
	config := ServerConfig{Name: "mail", Endpoint: "https://mail.example/mcp", Authorization: oauthTestConfig("https://accounts.example/token")}
	config.Authorization.RedirectURL = "https://app.example/mcp/mail/callback"
	require.NoError(t, store.Put(t.Context(), "alice", config))
	require.NoError(t, store.Put(t.Context(), "alice", ServerConfig{Name: "docs", Endpoint: "https://docs.example/mcp"}))
	provider := authorizationProvider(t)
	alice := WithNamespaceResolver(func(*http.Request) (string, error) { return "alice", nil })
	list := func(handler http.Handler) map[string]ConnectorInfo {
		t.Helper()
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
		require.Equal(t, http.StatusOK, recorder.Code)
		var infos []ConnectorInfo
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &infos))
		result := map[string]ConnectorInfo{}
		for _, info := range infos {
			result[info.Name] = info
		}
		return result
	}

	// Connection status appears only for OAuth servers, and only when OAuth routes exist.
	handler := NewHandler(store, alice, WithOAuth(provider))
	infos := list(handler)
	require.NotNil(t, infos["mail"].Connected)
	require.False(t, *infos["mail"].Connected)
	require.Nil(t, infos["docs"].Connected)
	require.Nil(t, list(NewHandler(store, alice))["mail"].Connected)

	stored := config
	stored.Namespace = "alice"
	key := OAuthCredentialKey(connectorFor(stored))
	require.NoError(t, provider.store.Save(t.Context(), "alice", key, &oauth2.Token{AccessToken: "alice-token"}))
	require.True(t, *list(handler)["mail"].Connected)

	// Deleting the server also forgets the user's grant for it.
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/mail", nil))
	require.Equal(t, http.StatusNoContent, recorder.Code)
	_, err := provider.store.Load(t.Context(), "alice", key)
	require.ErrorIs(t, err, ErrCredentialNotFound)
	require.NotContains(t, list(handler), "mail")
}
