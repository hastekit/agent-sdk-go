package mcpclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

// oauthTestProvider shares real file persistence and explicit local OAuth endpoints.
func oauthTestProvider(t *testing.T, tokenURL string) (*OAuthCredentialProvider, *FileCredentialStore) {
	t.Helper()
	store, err := NewFileCredentialStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	provider, err := NewOAuthCredentialProvider(OAuthCredentialProviderConfig{
		Store: store,
	})
	require.NoError(t, err)
	return provider, store
}

// oauthTestConfig supplies settings directly, without a provider-side server registry.
func oauthTestConfig(tokenURL string) *OAuthConfig {
	return &OAuthConfig{ClientID: "client", ClientSecret: "secret", RedirectURL: "http://localhost/auth/mcp/gmail/callback", AuthURL: "https://accounts.example/authorize", TokenURL: tokenURL, AuthStyle: oauth2.AuthStyleInParams, Scopes: []string{"read"}}
}

// oauthTestKey names the stored grant for a global test connector.
func oauthTestKey(name, tokenURL string) string {
	return OAuthCredentialKey(Connector{Name: name, Authorization: oauthTestConfig(tokenURL)})
}

func oauthTestStore(tokenURL string) *Store {
	return NewMemoryStore().WithMCPServerConfig([]ServerConfig{
		{Name: "gmail", Endpoint: "https://gmail.example/mcp", Authorization: oauthTestConfig(tokenURL)},
		{Name: "calendar", Endpoint: "https://gmail.example/mcp", Authorization: oauthTestConfig(tokenURL)},
	})
}

// Concurrent requests refresh once and persist rotated refresh tokens without changing principal identity.
func TestOAuthCredentialProviderRefreshPersistence(t *testing.T) {
	var refreshes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Observe the real OAuth request without exposing tokens in response errors.
		_ = r.ParseForm()
		assert.Equal(t, "refresh_token", r.Form.Get("grant_type"))
		assert.Equal(t, "old-refresh", r.Form.Get("refresh_token"))
		refreshes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"rotated-refresh","token_type":"Bearer","expires_in":3600}`))
	}))
	defer server.Close()
	provider, store := oauthTestProvider(t, server.URL)
	gmail := oauthTestKey("gmail", server.URL)
	require.NoError(t, store.Save(t.Context(), "ada", gmail, &oauth2.Token{AccessToken: "expired", RefreshToken: "old-refresh", Expiry: time.Now().Add(-time.Hour)}))

	// Separate token sources use the same namespace/server lock and latest persisted credential.
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			credential, err := provider.Resolve(t.Context(), "ada", Connector{Name: "gmail", Endpoint: "https://gmail.example/mcp", Authorization: oauthTestConfig(server.URL)}, nil)
			if !assert.NoError(t, err) {
				return
			}
			assert.Equal(t, "ada", credential.Principal)
			token, err := credential.TokenSource.Token()
			if assert.NoError(t, err) {
				assert.Equal(t, "new-access", token.AccessToken)
			}
		})
	}
	wg.Wait()
	require.EqualValues(t, 1, refreshes.Load())
	token, err := store.Load(t.Context(), "ada", gmail)
	require.NoError(t, err)
	require.Equal(t, "rotated-refresh", token.RefreshToken)

	// A source remains usable after the request that created it ends, and sees reauthorized credentials.
	ctx, cancel := context.WithCancel(t.Context())
	credential, err := provider.Resolve(ctx, "ada", Connector{Name: "gmail", Endpoint: "https://gmail.example/mcp", Authorization: oauthTestConfig(server.URL)}, nil)
	require.NoError(t, err)
	cancel()
	require.NoError(t, store.Save(t.Context(), "ada", gmail, &oauth2.Token{AccessToken: "reconnected"}))
	token, err = credential.TokenSource.Token()
	require.NoError(t, err)
	require.Equal(t, "reconnected", token.AccessToken)
	require.NoError(t, store.Delete(t.Context(), "ada", gmail))
	_, err = credential.TokenSource.Token()
	require.ErrorIs(t, err, ErrCredentialNotFound)
}

// Namespace and destination binding prevent another user or server from borrowing a credential.
func TestOAuthCredentialProviderIsolation(t *testing.T) {
	provider, store := oauthTestProvider(t, "https://accounts.example/token")
	tokenURL := "https://accounts.example/token"
	require.NoError(t, store.Save(t.Context(), "ada", oauthTestKey("gmail", tokenURL), &oauth2.Token{AccessToken: "ada-gmail"}))
	require.NoError(t, store.Save(t.Context(), "grace", oauthTestKey("gmail", tokenURL), &oauth2.Token{AccessToken: "grace-gmail"}))
	require.NoError(t, store.Save(t.Context(), "ada", oauthTestKey("calendar", tokenURL), &oauth2.Token{AccessToken: "ada-calendar"}))
	for _, pair := range [][2]string{{"ada", "gmail"}, {"grace", "gmail"}, {"ada", "calendar"}} {
		credential, err := provider.Resolve(t.Context(), pair[0], Connector{Name: pair[1], Endpoint: "https://gmail.example/mcp", Authorization: oauthTestConfig("https://accounts.example/token")}, map[string]any{"namespace": "spoofed"})
		require.NoError(t, err)
		token, err := credential.TokenSource.Token()
		require.NoError(t, err)
		require.Equal(t, pair[0]+"-"+pair[1], token.AccessToken)
	}

	// Missing identity is an auth error; missing OAuth settings fail before reading tokens.
	_, err := provider.Resolve(t.Context(), "", Connector{Name: "gmail", Endpoint: "https://gmail.example/mcp", Authorization: oauthTestConfig("https://accounts.example/token")}, map[string]any{"namespace": "ada"})
	var authError *agents.ToolsetError
	require.ErrorAs(t, err, &authError)
	require.Equal(t, agents.ToolsetErrorAuth, authError.Kind)
	_, err = provider.Resolve(t.Context(), "ada", Connector{Name: "gmail", Endpoint: "https://attacker.example/mcp"}, nil)
	require.Error(t, err)
	_, err = provider.Resolve(t.Context(), "unknown", Connector{Name: "gmail", Endpoint: "https://gmail.example/mcp", Authorization: oauthTestConfig("https://accounts.example/token")}, nil)
	require.ErrorIs(t, err, ErrCredentialNotFound)
}

// Failed refreshes cannot leak token endpoint response bodies into agent-visible errors.
func TestOAuthRefreshErrorRedaction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "secret-access-token", http.StatusBadRequest)
	}))
	defer server.Close()
	provider, store := oauthTestProvider(t, server.URL)
	require.NoError(t, store.Save(t.Context(), "ada", oauthTestKey("gmail", server.URL), &oauth2.Token{AccessToken: "old", RefreshToken: "refresh", Expiry: time.Now().Add(-time.Hour)}))
	credential, err := provider.Resolve(t.Context(), "ada", Connector{Name: "gmail", Endpoint: "https://gmail.example/mcp", Authorization: oauthTestConfig(server.URL)}, nil)
	require.NoError(t, err)
	_, err = credential.TokenSource.Token()
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret-access-token")
	_, err = store.Load(t.Context(), "ada", oauthTestKey("gmail", server.URL))
	require.False(t, errors.Is(err, ErrCredentialNotFound))
}
