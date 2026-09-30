package mcpclient

import (
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

// authorizationProvider creates shared token persistence without registering server names.
func authorizationProvider(t *testing.T) *OAuthCredentialProvider {
	t.Helper()
	store, err := NewFileCredentialStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	provider, err := NewOAuthCredentialProvider(OAuthCredentialProviderConfig{Store: store})
	require.NoError(t, err)
	return provider
}

func TestOAuthSettingsComeFromServerConfig(t *testing.T) {
	// A provider resolves any configured server without a registration phase.
	provider := authorizationProvider(t)
	require.NoError(t, provider.store.Save(t.Context(), "alice", oauthTestKey("mail", "https://accounts.example/token"), &oauth2.Token{AccessToken: "alice-token"}))
	connector := Connector{Name: "mail", Endpoint: "https://mail.example/mcp", Authorization: oauthTestConfig("https://accounts.example/token")}
	credential, err := provider.Resolve(t.Context(), "alice", connector, nil)
	require.NoError(t, err)
	token, err := credential.TokenSource.Token()
	require.NoError(t, err)
	require.Equal(t, "alice-token", token.AccessToken)
	require.Equal(t, "alice", credential.Principal)

	// OAuth still requires complete settings and a secure resource endpoint.
	connector.Authorization = nil
	_, err = provider.Resolve(t.Context(), "alice", connector, nil)
	require.ErrorContains(t, err, "authorization config")
	connector.Authorization = oauthTestConfig("https://accounts.example/token")
	connector.Endpoint = "http://remote.example/mcp"
	_, err = provider.Resolve(t.Context(), "alice", connector, nil)
	require.ErrorContains(t, err, "HTTPS")
}
