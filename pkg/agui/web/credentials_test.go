package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents/mcpclient"
	"github.com/hastekit/agent-sdk-go/pkg/agui"
	"github.com/stretchr/testify/require"
)

func TestMCPCredentialsThroughWebHandler(t *testing.T) {
	// Exercise a real code exchange without contacting an external OAuth service.
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"private-access","refresh_token":"private-refresh","token_type":"Bearer","expires_in":3600}`))
	}))
	defer tokenServer.Close()
	store, err := mcpclient.NewFileCredentialStore(t.TempDir())
	require.NoError(t, err)
	defer store.Close()
	const callbackURL = "http://localhost:8070/api/agui/mcp/gmail/callback"
	provider, err := mcpclient.NewOAuthCredentialProvider(mcpclient.OAuthCredentialProviderConfig{Store: store})
	require.NoError(t, err)

	// OAuth settings are available to HTTP routes before any agent discovery.
	configs := mcpclient.NewMemoryStore().WithMCPServerConfig([]mcpclient.ServerConfig{{
		Name: "gmail", Endpoint: "https://mcp.example/gmail",
		Authorization: &mcpclient.OAuthConfig{ClientID: "client", RedirectURL: callbackURL, AuthURL: "https://accounts.example/authorize", TokenURL: tokenServer.URL},
	}})

	// Use the same resolver as chat and verify it receives the matched server exactly once.
	resolutions := 0
	handler := Handler(registry{}, agui.WithMCPStore(configs, mcpclient.WithOAuth(provider)), agui.WithNamespaceResolver(func(r *http.Request) (string, error) {
		resolutions++
		require.NotEmpty(t, r.PathValue("server"))
		if r.Header.Get("Subject") == "denied" {
			return "", errors.New("private authentication failure")
		}
		return r.Header.Get("Subject"), nil
	}))
	request := func(path, subject string, cookie *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, APIPrefix+"/mcp/"+path, nil)
		req.Header.Set("Subject", subject)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		before := resolutions
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		require.Equal(t, before+1, resolutions)
		require.NotContains(t, response.Body.String(), "private-")
		return response
	}

	// The web mount preserves the complete registered callback URL and its cookie path.
	connect := request("gmail/connect", "alice", nil)
	require.Equal(t, http.StatusFound, connect.Code)
	location, err := url.Parse(connect.Header().Get("Location"))
	require.NoError(t, err)
	require.Equal(t, callbackURL, location.Query().Get("redirect_uri"))
	cookies := connect.Result().Cookies()
	require.Len(t, cookies, 1)
	require.Equal(t, APIPrefix+"/mcp/gmail/callback", cookies[0].Path)
	callback := "gmail/callback?code=code&state=" + url.QueryEscape(location.Query().Get("state"))

	// Reject unauthenticated and mismatched users before persisting an authorization.
	denied := request(callback, "denied", cookies[0])
	require.Equal(t, http.StatusForbidden, denied.Code)
	require.NotContains(t, denied.Body.String(), "private authentication failure")
	require.Equal(t, http.StatusBadRequest, request(callback, "bob", cookies[0]).Code)
	require.Equal(t, http.StatusNotFound, request("unknown/connect", "alice", nil).Code)

	// Store the result under the chat namespace and resolve it through the shared MCP provider.
	require.Equal(t, http.StatusOK, request(callback, "alice", cookies[0]).Code)
	credential, err := provider.Resolve(t.Context(), "alice", mcpclient.Connector{Name: "gmail", Endpoint: "https://mcp.example/gmail", Authorization: &mcpclient.OAuthConfig{ClientID: "client", RedirectURL: callbackURL, AuthURL: "https://accounts.example/authorize", TokenURL: tokenServer.URL}}, nil)
	require.NoError(t, err)
	token, err := credential.TokenSource.Token()
	require.NoError(t, err)
	require.Equal(t, "private-access", token.AccessToken)
	_, err = store.Load(t.Context(), "bob", "gmail")
	require.ErrorIs(t, err, mcpclient.ErrCredentialNotFound)
	require.Equal(t, http.StatusBadRequest, request(callback, "alice", cookies[0]).Code)
}

func TestMCPCredentialsDisabled(t *testing.T) {
	// Missing or explicitly nil providers leave OAuth routes unmounted.
	for _, opts := range [][]agui.Option{nil, {agui.WithMCPStore(nil)}} {
		handler := Handler(registry{}, opts...)
		for _, action := range []string{"connect", "callback"} {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, APIPrefix+"/mcp/gmail/"+action, nil))
			require.Equal(t, http.StatusNotFound, response.Code)
		}
	}
}
