package mcpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// uncheckedConfigStore represents persisted records supplied by an application adapter.
type uncheckedConfigStore struct {
	config ServerConfig
	writes int
}

func (s *uncheckedConfigStore) ListServerConfigs(context.Context, string, map[string]any) ([]ServerConfig, error) {
	return []ServerConfig{s.config}, nil
}

func (s *uncheckedConfigStore) Put(_ context.Context, _ string, config ServerConfig) error {
	s.writes++
	s.config = config
	return nil
}

func (*uncheckedConfigStore) Delete(context.Context, string, string) error { return nil }

func TestUserTemplatesRejectedAtEveryConfigurationBoundary(t *testing.T) {
	// Nested and typed metadata must receive the same checks as ordinary header strings.
	cases := map[string]ServerConfig{
		"header":       {Headers: map[string]string{"X-Token": "Bearer {{github_token}}"}},
		"meta":         {Meta: map[string]any{"token": "{{github_token}}"}},
		"nested map":   {Meta: map[string]any{"nested": mcp.Meta{"token": "{{github_token}}"}}},
		"typed map":    {Meta: map[string]any{"nested": map[string]string{"token": "{{github_token}}"}}},
		"array":        {Meta: map[string]any{"nested": []any{42, map[string]any{"token": "{{github_token}}"}}}},
		"typed array":  {Meta: map[string]any{"nested": []string{"literal", "{{github_token}}"}}},
		"template key": {Meta: map[string]any{"{{github_token}}": "literal"}},
	}
	for name, config := range cases {
		t.Run(name, func(t *testing.T) {
			config.Name, config.Endpoint = "user", "https://mcp.example/mcp"
			file, err := NewFileStore(filepath.Join(t.TempDir(), "servers.json"))
			require.NoError(t, err)
			custom := &uncheckedConfigStore{}
			for _, store := range []*Store{NewMemoryStore(), file, NewStore(custom)} {
				require.ErrorContains(t, store.Put(t.Context(), "alice", config), "cannot contain templates")
			}
			require.Zero(t, custom.writes, "validation must happen before custom persistence")

			// Inline definitions, existing files, and custom store reads cannot bypass validation.
			config.Namespace = "alice"
			inline := NewMemoryStore().WithMCPServerConfig([]ServerConfig{config})
			_, err = inline.ListServerConfigs(t.Context(), "alice", nil)
			require.ErrorContains(t, err, "cannot contain templates")
			// Persisted user records never connect; they fail only their own connector.
			custom.config = config
			path := filepath.Join(t.TempDir(), "unsafe.json")
			data, err := json.Marshal([]ServerConfig{config})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, data, 0600))
			existing, err := NewFileStore(path)
			require.NoError(t, err, "one user's record must not fail the whole file")
			for _, source := range []MCPServerConfigStore{custom, existing} {
				statuses, tools, err := NewClient(source).ListTools(t.Context(), "alice", map[string]any{"github_token": "secret"})
				require.NoError(t, err)
				require.Empty(t, tools)
				require.Len(t, statuses, 1)
				require.False(t, statuses[0].Connected)
				require.Contains(t, statuses[0].Detail, "cannot contain templates")
			}

			// Ownership comes from Put's namespace argument, so globals still support templates.
			require.NoError(t, NewMemoryStore().Put(t.Context(), "", config))
		})
	}
}

func TestHandlerRejectsUnsafeUserConfigsWithoutPersisting(t *testing.T) {
	// HTTP registration ignores forged global ownership and rejects unsafe definitions before storage.
	store := NewMemoryStore()
	handler := NewHandler(store, WithNamespaceResolver(func(*http.Request) (string, error) { return "alice", nil }))
	for _, body := range []string{
		`{"namespace":"","endpoint":"https://evil.example/mcp","headers":{"x":"{{github_token}}"}}`,
		`{"endpoint":"https://evil.example/mcp","meta":{"nested":[{"token":"\u007b\u007bgithub_token\u007d\u007d"}]}}`,
		`{"endpoint":"https://evil.example/mcp","meta":{"token":"{{ .github_token }}"}}`,
		`{"endpoint":"http://169.254.169.254/latest/meta-data/"}`,
		`{"endpoint":"https://127.0.0.1/mcp"}`,
		`{"endpoint":"http://[::ffff:127.0.0.1]/mcp"}`,
		`{"endpoint":"https://mcp.example","env":{"TOKEN":"value"}}`,
		`{"transport":"stdio","command":["sh"]}`,
		`{"endpoint":"https://mcp.example","authorization":{"clientId":"client","authUrl":"https://auth.example","tokenUrl":"http://localhost/token","redirectUrl":"https://app.example/callback"}}`,
		`{"endpoint":"https://mcp.example","authorization":{"clientId":"client","authUrl":"https://auth.example","tokenUrl":"https://169.254.169.254/token","redirectUrl":"https://app.example/callback"}}`,
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("PUT", "/user", strings.NewReader(body)))
		require.Equal(t, http.StatusBadRequest, response.Code, body)
		configs, err := store.ListServerConfigs(t.Context(), "alice", nil)
		require.NoError(t, err)
		require.Empty(t, configs)
	}

	// Literal user-supplied credentials and metadata remain supported.
	body := `{"endpoint":"https://mcp.example","headers":{"Authorization":"Bearer user-owned-token"},"meta":{"nested":[{"value":"literal"}]}}`
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("PUT", "/user", strings.NewReader(body)))
	require.Equal(t, http.StatusNoContent, response.Code)
	configs, err := store.ListServerConfigs(t.Context(), "alice", nil)
	require.NoError(t, err)
	require.Len(t, configs, 1)
	require.Equal(t, "alice", configs[0].Namespace)
}

func TestUserOAuthRejectsPrivateURLsAndGlobalAllowsLocalDevelopment(t *testing.T) {
	// Check every URL this process requests at registration, not just the MCP endpoint.
	for _, field := range []string{"endpoint", "token"} {
		t.Run(field, func(t *testing.T) {
			config := ServerConfig{Name: "mail", Namespace: "alice", Endpoint: "https://mcp.example", Authorization: oauthTestConfig("https://auth.example/token")}
			config.Authorization.RedirectURL = "https://app.example/callback"
			switch field {
			case "endpoint":
				config.Endpoint = "http://localhost/mcp"
			case "auth":
				config.Authorization.AuthURL = "http://localhost/authorize"
			case "token":
				config.Authorization.TokenURL = "http://localhost/token"
			case "redirect":
				config.Authorization.RedirectURL = "http://localhost/callback"
			}
			require.ErrorIs(t, validateServerConfig(config), errPrivateEndpoint)
			config.Namespace = ""
			require.NoError(t, validateServerConfig(config))
		})
	}

	// Browser-only URLs are never requested server-side, so a loopback callback works locally.
	config := ServerConfig{Name: "mail", Namespace: "alice", Endpoint: "https://mcp.example", Authorization: oauthTestConfig("https://auth.example/token")}
	config.Authorization.AuthURL = "http://localhost:9000/authorize"
	config.Authorization.RedirectURL = "http://localhost:8080/mcp/mail/callback"
	require.NoError(t, validateServerConfig(config))

	// They still need HTTPS off loopback.
	config.Authorization.RedirectURL = "http://app.example/mcp/mail/callback"
	require.ErrorContains(t, validateServerConfig(config), "requires HTTPS")
}
