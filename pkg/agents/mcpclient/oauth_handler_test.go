package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

// Browser authorization uses PKCE and state, then saves only under the authenticated namespace/server pair.
func TestOAuthHandlerAuthorization(t *testing.T) {
	var exchanges atomic.Int32
	var challenge atomic.Value
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Validate the code exchange against the challenge from this browser's authorization request.
		_ = r.ParseForm()
		assert.Equal(t, "authorization_code", r.Form.Get("grant_type"))
		assert.Equal(t, "authorization-code", r.Form.Get("code"))
		assert.Equal(t, challenge.Load(), oauth2.S256ChallengeFromVerifier(r.Form.Get("code_verifier")))
		exchanges.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"access","refresh_token":"refresh","token_type":"Bearer","expires_in":3600}`))
	}))
	defer tokenServer.Close()
	provider, store := oauthTestProvider(t, tokenServer.URL)
	handler := NewHandler(oauthTestStore(tokenServer.URL), WithOAuth(provider), WithNamespaceResolver(func(r *http.Request) (string, error) { return r.Header.Get("Subject"), nil }))

	// Starting consent sets a browser-bound cookie and never puts the namespace or secrets in state.
	request := httptest.NewRequest(http.MethodGet, "/gmail/connect", nil)
	request.Header.Set("Subject", "ada")
	connect := httptest.NewRecorder()
	handler.ServeHTTP(connect, request)
	require.Equal(t, http.StatusFound, connect.Code)
	location, err := url.Parse(connect.Header().Get("Location"))
	require.NoError(t, err)
	query := location.Query()
	challenge.Store(query.Get("code_challenge"))
	require.NotEmpty(t, challenge.Load())
	require.Equal(t, "S256", query.Get("code_challenge_method"))
	require.Equal(t, "offline", query.Get("access_type"))
	require.Equal(t, "client", query.Get("client_id"))
	require.Equal(t, oauthTestConfig("").RedirectURL, query.Get("redirect_uri"))
	require.NotContains(t, query.Get("state"), "ada")
	cookies := connect.Result().Cookies()
	require.Len(t, cookies, 1)
	require.True(t, cookies[0].HttpOnly)
	require.Equal(t, http.SameSiteLaxMode, cookies[0].SameSite)
	require.Equal(t, "/auth/mcp/gmail/callback", cookies[0].Path)

	// Another browser, namespace, or server cannot exchange this authorization attempt.
	callback := func(subject, server, state string, cookie *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/"+server+"/callback?code=authorization-code&state="+url.QueryEscape(state), nil)
		request.Header.Set("Subject", subject)
		if cookie != nil {
			request.AddCookie(cookie)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	state := query.Get("state")
	require.Equal(t, http.StatusBadRequest, callback("ada", "gmail", state, nil).Code)
	require.Equal(t, http.StatusBadRequest, callback("grace", "gmail", state, cookies[0]).Code)
	require.Equal(t, http.StatusBadRequest, callback("ada", "gmail", "wrong-state", cookies[0]).Code)
	require.Equal(t, http.StatusBadRequest, callback("ada", "calendar", state, cookies[0]).Code)
	calendarCookie := authorizationCookie("calendar", oauthTestConfig("").RedirectURL, state)
	require.Equal(t, http.StatusBadRequest, callback("ada", "calendar", state, calendarCookie).Code)
	require.Zero(t, exchanges.Load())

	// Completing the correct browser flow persists refreshable credentials and consumes the state.
	response := callback("ada", "gmail", state, cookies[0])
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	require.NotContains(t, response.Body.String(), "refresh")
	token, err := store.Load(t.Context(), "ada", oauthTestKey("gmail", tokenServer.URL))
	require.NoError(t, err)
	require.Equal(t, "refresh", token.RefreshToken)
	_, err = store.Load(t.Context(), "grace", oauthTestKey("gmail", tokenServer.URL))
	require.ErrorIs(t, err, ErrCredentialNotFound)
	require.Equal(t, http.StatusBadRequest, callback("ada", "gmail", state, cookies[0]).Code)
	require.EqualValues(t, 1, exchanges.Load())
}

// An app whose API authenticates with headers starts consent from a script and
// relays the authorization server's response: the attempt finishes only for
// the user who started it, and never through the cookie-checked GET callback.
func TestOAuthHandlerAuthorizationForScripts(t *testing.T) {
	var exchanges atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		assert.Equal(t, "authorization-code", r.Form.Get("code"))
		assert.NotEmpty(t, r.Form.Get("code_verifier"))
		exchanges.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"access","refresh_token":"refresh","token_type":"Bearer","expires_in":3600}`))
	}))
	defer tokenServer.Close()
	provider, store := oauthTestProvider(t, tokenServer.URL)
	handler := NewHandler(oauthTestStore(tokenServer.URL), WithOAuth(provider), WithNamespaceResolver(func(r *http.Request) (string, error) { return r.Header.Get("Subject"), nil }))

	start := func(server string) string {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/"+server+"/connect", nil)
		request.Header.Set("Subject", "ada")
		request.Header.Set("Accept", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		require.Equal(t, http.StatusOK, response.Code)
		require.Empty(t, response.Result().Cookies(), "a script's attempt sets no browser cookie")
		var body struct {
			AuthorizationURL string `json:"authorizationUrl"`
		}
		require.NoError(t, json.NewDecoder(response.Body).Decode(&body))
		location, err := url.Parse(body.AuthorizationURL)
		require.NoError(t, err)
		require.Equal(t, "S256", location.Query().Get("code_challenge_method"))
		require.Equal(t, oauthTestConfig("").RedirectURL, location.Query().Get("redirect_uri"))
		return location.Query().Get("state")
	}
	relay := func(subject, server string, body map[string]string) int {
		t.Helper()
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		request := httptest.NewRequest(http.MethodPost, "/"+server+"/callback", bytes.NewReader(encoded))
		request.Header.Set("Subject", subject)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response.Code
	}
	state := start("gmail")

	// The GET callback accepts only attempts that set its cookie, even a forged one.
	navigation := httptest.NewRequest(http.MethodGet, "/gmail/callback?code=authorization-code&state="+url.QueryEscape(state), nil)
	navigation.Header.Set("Subject", "ada")
	navigation.AddCookie(authorizationCookie("gmail", oauthTestConfig("").RedirectURL, state))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, navigation)
	require.Equal(t, http.StatusBadRequest, response.Code)

	// Another user, server, or state cannot finish it.
	code := map[string]string{"state": state, "code": "authorization-code"}
	require.Equal(t, http.StatusBadRequest, relay("grace", "gmail", code))
	require.Equal(t, http.StatusBadRequest, relay("ada", "calendar", code))
	require.Equal(t, http.StatusBadRequest, relay("ada", "gmail", map[string]string{"state": "wrong-state", "code": "authorization-code"}))
	require.Zero(t, exchanges.Load())

	// The user who started it does, once.
	require.Equal(t, http.StatusNoContent, relay("ada", "gmail", code))
	token, err := store.Load(t.Context(), "ada", oauthTestKey("gmail", tokenServer.URL))
	require.NoError(t, err)
	require.Equal(t, "refresh", token.RefreshToken)
	require.Equal(t, http.StatusBadRequest, relay("ada", "gmail", code))
	require.EqualValues(t, 1, exchanges.Load())

	// Refused consent ends the attempt without an exchange.
	refused := start("calendar")
	require.Equal(t, http.StatusBadRequest, relay("ada", "calendar", map[string]string{"state": refused, "error": "access_denied"}))
	require.Equal(t, http.StatusBadRequest, relay("ada", "calendar", map[string]string{"state": refused, "code": "authorization-code"}))
	require.EqualValues(t, 1, exchanges.Load())
}

// Expired state cannot be exchanged even by the browser and user that initiated it.
func TestOAuthHandlerRejectsExpiredState(t *testing.T) {
	provider, store := oauthTestProvider(t, "https://accounts.example/token")
	handler := &oauthHandler{
		provider:  provider,
		client:    NewClient(oauthTestStore("https://accounts.example/token")),
		namespace: func(*http.Request) (string, error) { return "ada", nil },
		pending: map[string]pendingAuthorization{
			"expired": {namespace: "ada", server: "gmail", expires: time.Now().Add(-time.Second), complete: func(context.Context, *auth.AuthorizationResult) error {
				t.Error("an expired attempt must not be completed")
				return nil
			}},
		},
	}
	request := httptest.NewRequest(http.MethodGet, "/gmail/callback?state=expired&code=unused", nil)
	request.SetPathValue("server", "gmail")
	request.AddCookie(authorizationCookie("gmail", oauthTestConfig("").RedirectURL, "expired"))
	response := httptest.NewRecorder()
	handler.callback(response, request)

	// Rejection happens before contacting the token endpoint or creating a credential record.
	require.Equal(t, http.StatusBadRequest, response.Code)
	_, err := store.Load(t.Context(), "ada", oauthTestKey("gmail", "https://accounts.example/token"))
	require.ErrorIs(t, err, ErrCredentialNotFound)
}

// CallToolDirect derives credential identity from the serialized call rather than free-form context.
func TestCallToolDirectUsesCallNamespace(t *testing.T) {
	var receivedNamespace string
	client := newTestClient(t, "https://example.test/mcp", withCredentials(CredentialProviderFn(
		func(ctx context.Context, namespace string, connector Connector, runContext map[string]any) (*Credential, error) {
			receivedNamespace = namespace
			require.Equal(t, "spoofed", runContext["namespace"])
			return nil, ErrCredentialNotFound
		})))
	runContext := map[string]any{"namespace": "spoofed"}
	call := echoCall("echo")
	call.Namespace = "authenticated-user"
	_, err := client.CallToolDirect(t.Context(), runContext, &agents.BaseTool{Name: "echo"}, call)
	require.ErrorIs(t, err, ErrCredentialNotFound)
	require.Equal(t, "authenticated-user", receivedNamespace)
	require.Equal(t, "spoofed", runContext["namespace"])
}

func TestListingAndInvalidationPassNamespaceToProvider(t *testing.T) {
	// Authentication must receive an explicit subject while application metadata remains unchanged.
	for _, operation := range []string{"list", "invalidate"} {
		t.Run(operation, func(t *testing.T) {
			var receivedNamespace string
			client := newTestClient(t, "https://example.test/mcp", withSchemaCache(newMemCache()), withCredentials(CredentialProviderFn(
				func(_ context.Context, namespace string, _ Connector, rc map[string]any) (*Credential, error) {
					receivedNamespace = namespace
					require.Equal(t, "spoofed", rc["namespace"])
					return nil, ErrCredentialNotFound
				})))
			rc := map[string]any{"namespace": "spoofed"}

			// Stop at credential resolution so this test never contacts an MCP server.
			var err error
			if operation == "list" {
				_, err = client.ListTools(t.Context(), "authenticated-user", rc)
			} else {
				err = client.InvalidateToolCache(t.Context(), "authenticated-user", rc)
			}
			require.ErrorIs(t, err, ErrCredentialNotFound)
			require.Equal(t, "authenticated-user", receivedNamespace)
			require.Equal(t, "spoofed", rc["namespace"])
		})
	}
}
