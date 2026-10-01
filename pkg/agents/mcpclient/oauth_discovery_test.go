package mcpclient

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

// authServer is an MCP server and its authorization server, publishing the
// metadata the MCP authorization spec discovers and accepting registrations.
type authServer struct {
	*httptest.Server
	registrations atomic.Int32
	exchanges     atomic.Int32
	challenge     atomic.Value
	// noRegistration leaves the registration endpoint out of the metadata.
	noRegistration bool
}

func newAuthServer(t *testing.T, noRegistration bool) *authServer {
	t.Helper()
	s := &authServer{noRegistration: noRegistration}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := s.URL
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/mcp":
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource/mcp", scope="notes.read"`, base))
			w.WriteHeader(http.StatusUnauthorized)
		case "/.well-known/oauth-protected-resource/mcp":
			_ = json.NewEncoder(w).Encode(map[string]any{"resource": base + "/mcp", "authorization_servers": []string{base}})
		case "/.well-known/oauth-authorization-server":
			meta := map[string]any{
				"issuer": base, "authorization_endpoint": base + "/authorize", "token_endpoint": base + "/token",
				"code_challenge_methods_supported": []string{"S256"}, "scopes_supported": []string{"notes.read", "offline_access"},
			}
			if !s.noRegistration {
				meta["registration_endpoint"] = base + "/register"
			}
			_ = json.NewEncoder(w).Encode(meta)
		case "/register":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			assert.Equal(t, []any{"http://localhost/auth/mcp/notes/callback"}, body["redirect_uris"])
			assert.Equal(t, "none", body["token_endpoint_auth_method"])
			n := s.registrations.Add(1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"client_id": fmt.Sprintf("registered-%d", n), "redirect_uris": body["redirect_uris"], "token_endpoint_auth_method": "none",
			})
		case "/token":
			_ = r.ParseForm()
			assert.Equal(t, base+"/mcp", r.Form.Get("resource"), "the grant is bound to the MCP server")
			switch r.Form.Get("grant_type") {
			case "authorization_code":
				assert.Equal(t, "registered-1", r.Form.Get("client_id"))
				assert.Equal(t, s.challenge.Load(), oauth2.S256ChallengeFromVerifier(r.Form.Get("code_verifier")))
				s.exchanges.Add(1)
				_, _ = w.Write([]byte(`{"access_token":"access","refresh_token":"refresh","token_type":"Bearer","expires_in":3600}`))
			default:
				w.WriteHeader(http.StatusBadRequest)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func discoveredConfig(endpoint string) ServerConfig {
	return ServerConfig{Name: "notes", Endpoint: endpoint, Authorization: &OAuthConfig{RedirectURL: "http://localhost/auth/mcp/notes/callback"}}
}

// A server configured with only its URL: the flow discovers its authorization
// server, registers a client there, and authorizes with PKCE and the resource
// indicator. The registration is kept and reused, and runs refresh through it.
func TestOAuthHandlerDiscoversAndRegisters(t *testing.T) {
	as := newAuthServer(t, false)
	provider, store := oauthTestProvider(t, "")
	config := discoveredConfig(as.URL + "/mcp")
	handler := NewHandler(NewMemoryStore().WithMCPServerConfig([]ServerConfig{config}), WithOAuth(provider),
		WithNamespaceResolver(func(r *http.Request) (string, error) { return "ada", nil }))

	connect := func() (*url.URL, *http.Cookie) {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/notes/connect", nil))
		require.Equal(t, http.StatusFound, response.Code, response.Body.String())
		location, err := url.Parse(response.Header().Get("Location"))
		require.NoError(t, err)
		require.Len(t, response.Result().Cookies(), 1)
		return location, response.Result().Cookies()[0]
	}

	location, cookie := connect()
	query := location.Query()
	require.Equal(t, as.URL+"/authorize", location.Scheme+"://"+location.Host+location.Path)
	require.Equal(t, "registered-1", query.Get("client_id"))
	require.Equal(t, as.URL+"/mcp", query.Get("resource"))
	require.Equal(t, "S256", query.Get("code_challenge_method"))
	require.ElementsMatch(t, []string{"notes.read", "offline_access"}, strings.Fields(query.Get("scope")))
	as.challenge.Store(query.Get("code_challenge"))

	request := httptest.NewRequest(http.MethodGet, "/notes/callback?code=authorization-code&state="+url.QueryEscape(query.Get("state")), nil)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.EqualValues(t, 1, as.exchanges.Load())

	// The grant and the client that holds it are both kept.
	key := OAuthCredentialKey(connectorFor(config))
	token, err := store.Load(t.Context(), "ada", key)
	require.NoError(t, err)
	require.Equal(t, "refresh", token.RefreshToken)
	client, err := store.LoadOAuthClient(t.Context(), "ada", key)
	require.NoError(t, err)
	require.Equal(t, "registered-1", client.ClientID)
	require.Equal(t, as.URL+"/token", client.TokenURL)

	// Runs use it.
	credential, err := provider.Resolve(t.Context(), "ada", connectorFor(config), nil)
	require.NoError(t, err)
	current, err := credential.TokenSource.Token()
	require.NoError(t, err)
	require.Equal(t, "access", current.AccessToken)

	// Connecting again reuses the registration.
	location, _ = connect()
	require.Equal(t, "registered-1", location.Query().Get("client_id"))
	require.EqualValues(t, 1, as.registrations.Load())
}

// A server that offers no registration, with no client configured, cannot be
// signed in to, and says so before sending the user anywhere.
func TestOAuthHandlerWithoutRegistration(t *testing.T) {
	as := newAuthServer(t, true)
	provider, _ := oauthTestProvider(t, "")
	handler := NewHandler(NewMemoryStore().WithMCPServerConfig([]ServerConfig{discoveredConfig(as.URL + "/mcp")}), WithOAuth(provider),
		WithNamespaceResolver(func(r *http.Request) (string, error) { return "ada", nil }))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/notes/connect", nil))
	require.Equal(t, http.StatusBadGateway, response.Code)
	require.Empty(t, response.Header().Get("Location"))
	require.Zero(t, as.registrations.Load())
}

// Discovered configs leave out the endpoints, and may leave out the client;
// pinned endpoints still need both, and a client ID.
func TestOAuthConfigValidation(t *testing.T) {
	endpoint := "https://notes.example/mcp"
	valid := func(auth *OAuthConfig) error {
		_, err := resolveOAuthConfig(Connector{Name: "notes", Endpoint: endpoint, Authorization: auth})
		return err
	}
	redirect := "http://localhost/auth/mcp/notes/callback"
	require.NoError(t, valid(&OAuthConfig{RedirectURL: redirect}))
	require.NoError(t, valid(&OAuthConfig{RedirectURL: redirect, ClientID: "mine", ClientSecret: "secret"}))
	require.NoError(t, valid(&OAuthConfig{RedirectURL: redirect, ClientID: "mine", AuthURL: "https://a.example/authorize", TokenURL: "https://a.example/token"}))
	require.Error(t, valid(&OAuthConfig{RedirectURL: redirect, AuthURL: "https://a.example/authorize", TokenURL: "https://a.example/token"}), "pinned endpoints need a client")
	require.Error(t, valid(&OAuthConfig{RedirectURL: redirect, ClientID: "mine", TokenURL: "https://a.example/token"}), "both endpoints or neither")
	require.Error(t, valid(&OAuthConfig{RedirectURL: redirect, ClientSecret: "secret"}), "a secret needs its client")

	// A discovered grant is bound to the MCP endpoint that decides its authorization server.
	a := OAuthCredentialKey(Connector{Name: "notes", Endpoint: endpoint, Authorization: &OAuthConfig{RedirectURL: redirect}})
	b := OAuthCredentialKey(Connector{Name: "notes", Endpoint: "https://other.example/mcp", Authorization: &OAuthConfig{RedirectURL: redirect}})
	require.NotEqual(t, a, b)
}
