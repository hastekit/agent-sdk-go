package mcpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

// recordStore returns persisted records verbatim, like a database adapter that skips validation.
type recordStore struct {
	mu      sync.Mutex
	records []ServerConfig
}

func (s *recordStore) ListServerConfigs(_ context.Context, namespace string, _ map[string]any) ([]ServerConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := []ServerConfig{}
	for _, record := range s.records {
		if record.Namespace == "" || record.Namespace == namespace {
			result = append(result, record)
		}
	}
	return result, nil
}

func (s *recordStore) Put(_ context.Context, namespace string, config ServerConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	config.Namespace = namespace
	s.records = append(s.records, config)
	return nil
}

func (s *recordStore) Delete(_ context.Context, namespace, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.records[:0]
	for _, record := range s.records {
		if record.Namespace != namespace || record.Name != name {
			kept = append(kept, record)
		}
	}
	s.records = kept
	return nil
}

func statusNames(statuses []agents.ConnectorStatus) []string {
	names := make([]string, 0, len(statuses))
	for _, status := range statuses {
		names = append(names, status.Name)
	}
	return names
}

func TestUserConfigsCannotUseSharedCredentials(t *testing.T) {
	// Registration rejects the flag for users while developer-owned globals keep it.
	config := catalogConfig("vault", "https://vault.example/mcp")
	config.UseCredentials = true
	require.ErrorContains(t, NewMemoryStore().Put(t.Context(), "alice", config), "shared credential provider")
	require.NoError(t, NewMemoryStore().Put(t.Context(), "", config))

	// HTTP registration fails before persistence.
	store := NewMemoryStore()
	handler := NewHandler(store, WithNamespaceResolver(func(*http.Request) (string, error) { return "alice", nil }))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/vault", strings.NewReader(`{"endpoint":"https://vault.example/mcp","useCredentials":true}`)))
	require.Equal(t, http.StatusBadRequest, response.Code)
	persisted, err := store.ListServerConfigs(t.Context(), "alice", nil)
	require.NoError(t, err)
	require.Empty(t, persisted)

	// A record that bypassed registration never reaches the application's provider.
	var calls atomic.Int32
	provider := CredentialProviderFn(func(context.Context, string, Connector, map[string]any) (*Credential, error) {
		calls.Add(1)
		return &Credential{TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "application-secret"})}, nil
	})
	config.Namespace = "alice"
	statuses, tools, err := NewClient(&recordStore{records: []ServerConfig{config}}).WithCredentials(provider).ListTools(t.Context(), "alice", nil)
	require.NoError(t, err)
	require.Empty(t, tools)
	require.Len(t, statuses, 1)
	require.Contains(t, statuses[0].Detail, "shared credential provider")
	require.Zero(t, calls.Load())
}

func TestInvalidUserRecordsFailOnlyTheirConnector(t *testing.T) {
	// A database may hold records written before a validation rule existed, or duplicates.
	global := catalogConfig("docs", catalogServer(t, "global"))
	good := catalogConfig("good", userCatalogServer(t, "good"))
	good.Namespace = "alice"
	private := catalogConfig("private", "http://10.0.0.5/mcp")
	private.Namespace = "alice"
	duplicate := catalogConfig("dup", "https://dup.example/mcp")
	duplicate.Namespace = "alice"
	shadowed := catalogConfig("docs", "http://10.0.0.6/mcp")
	shadowed.Namespace = "alice"
	source := &recordStore{records: []ServerConfig{global, good, private, duplicate, duplicate, shadowed}}
	client := NewClient(source)

	// Usable connectors keep working; each bad record fails alone, and a global shadows a bad one.
	statuses, tools, err := client.ListTools(t.Context(), "alice", nil)
	require.NoError(t, err)
	require.Len(t, tools, 6)
	require.Equal(t, []string{"docs", "dup", "good", "private"}, statusNames(statuses))
	require.True(t, statuses[0].Connected)
	require.Contains(t, statuses[1].Detail, "duplicate")
	require.True(t, statuses[2].Connected)
	require.Contains(t, statuses[3].Detail, "public network addresses")

	// Disabling an unusable connector removes its status like any other.
	statuses, _, err = client.ListTools(t.Context(), "alice", nil, agents.MCPSelection{Disable: []string{"dup", "private"}})
	require.NoError(t, err)
	require.Equal(t, []string{"docs", "good"}, statusNames(statuses))

	// Calls report the configuration problem instead of connecting.
	call := &agents.ToolCall{Namespace: "alice", FunctionCallMessage: &responses.FunctionCallMessage{Name: "private__read", Arguments: "{}"}}
	_, err = client.CallTool(t.Context(), &agents.BaseTool{Name: "read", MCPServerName: "private"}, call)
	require.ErrorContains(t, err, "invalid MCP server configuration")

	// The owner can still see why, and delete the records over HTTP.
	handler := NewHandler(NewStore(source), WithNamespaceResolver(func(*http.Request) (string, error) { return "alice", nil }))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusOK, response.Code)
	var listed []ConnectorInfo
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &listed))
	problems := map[string]string{}
	for _, info := range listed {
		problems[info.Name] = info.Error
		if info.Name == "private" {
			require.False(t, info.ReadOnly)
		}
	}
	require.Empty(t, problems["good"])
	require.Contains(t, problems["private"], "public network addresses")
	for _, name := range []string{"dup", "private"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/"+name, nil))
		require.Equal(t, http.StatusNoContent, response.Code, name)
	}
	statuses, _, err = client.ListTools(t.Context(), "alice", nil)
	require.NoError(t, err)
	require.Equal(t, []string{"docs", "good"}, statusNames(statuses))

	// A broken global is the developer's configuration and still fails loudly.
	source.records = append(source.records, ServerConfig{Name: "broken"})
	_, _, err = client.ListTools(t.Context(), "alice", nil)
	require.ErrorContains(t, err, "requires an endpoint")
}

func TestFileStoreIsolatesBadUserRecords(t *testing.T) {
	// One user's unsafe record neither fails loading nor blocks the writes that repair it.
	path := filepath.Join(t.TempDir(), "servers.json")
	data, err := json.Marshal([]ServerConfig{
		{Name: "docs", Endpoint: "https://docs.example/mcp"},
		{Name: "bad", Namespace: "alice", Endpoint: "http://127.0.0.1/mcp"},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0600))
	store, err := NewFileStore(path)
	require.NoError(t, err)
	bob, err := store.ListServerConfigs(t.Context(), "bob", nil)
	require.NoError(t, err)
	require.Len(t, bob, 1)
	resolved, err := NewClient(store).listServerConfigs(t.Context(), "alice", nil)
	require.NoError(t, err)
	require.Contains(t, resolved.invalid["bad"].Error(), "public network addresses")
	require.NoError(t, store.Put(t.Context(), "alice", catalogConfig("ok", "https://ok.example/mcp")))
	require.NoError(t, store.Delete(t.Context(), "alice", "bad"))
	resolved, err = NewClient(store).listServerConfigs(t.Context(), "alice", nil)
	require.NoError(t, err)
	require.Empty(t, resolved.invalid)
	require.Contains(t, resolved.configs, "ok")

	// A broken global still fails the file at load time.
	require.NoError(t, os.WriteFile(path, []byte(`[{"name":"docs"}]`), 0600))
	_, err = NewFileStore(path)
	require.ErrorContains(t, err, "requires an endpoint")
}

func TestUserToolCollisionsFailOnlyThatConnector(t *testing.T) {
	store := NewMemoryStore()
	global := catalogConfig("docs", catalogServer(t, "global"))
	global.ToolPrefix = ""
	require.NoError(t, store.Put(t.Context(), "", global))

	// "a-user" sorts before "docs" but cannot claim a global's tool names.
	first := catalogConfig("a-user", userCatalogServer(t, "first"))
	first.ToolPrefix = ""
	mine := catalogConfig("mine", userCatalogServer(t, "mine"))
	mine.ToolPrefix = "shared__"
	other := catalogConfig("other", userCatalogServer(t, "other"))
	other.ToolPrefix = "shared__"
	for _, config := range []ServerConfig{first, mine, other} {
		require.NoError(t, store.Put(t.Context(), "alice", config))
	}
	client := NewClient(store)
	statuses, tools, err := client.ListTools(t.Context(), "alice", nil)
	require.NoError(t, err)
	require.Len(t, tools, 6)
	require.Equal(t, []string{"docs", "a-user", "mine", "other"}, statusNames(statuses))
	require.True(t, statuses[0].Connected)
	require.Contains(t, statuses[1].Detail, "conflicts with another connector")
	require.True(t, statuses[2].Connected)
	require.Contains(t, statuses[3].Detail, "conflicts with another connector")
	var servers []string
	for _, tool := range tools {
		servers = append(servers, tool.GetToolDescriptor().MCPServerName)
	}
	require.ElementsMatch(t, []string{"docs", "docs", "docs", "mine", "mine", "mine"}, servers)

	// Globals that collide are the developer's to fix and still fail the listing.
	clash := catalogConfig("zz", catalogServer(t, "zz"))
	clash.ToolPrefix = ""
	require.NoError(t, store.Put(t.Context(), "", clash))
	_, _, err = client.ListTools(t.Context(), "alice", nil)
	require.ErrorContains(t, err, "duplicate MCP tool")
}

func TestOAuthCredentialKeyBindsOwnerAndClient(t *testing.T) {
	// Owner, OAuth client, and token endpoint select the grant; other settings do not.
	base := Connector{Name: "mail", Endpoint: "https://mail.example/mcp", Authorization: oauthTestConfig("https://accounts.example/token")}
	user := base
	userAuth := *base.Authorization
	userAuth.RedirectURL = "https://app.example/callback"
	user.Namespace, user.Authorization = "alice", &userAuth
	otherClient := base
	otherClient.Authorization = oauthTestConfig("https://accounts.example/token")
	otherClient.Authorization.ClientID = "other"
	otherTokenURL := base
	otherTokenURL.Authorization = oauthTestConfig("https://other.example/token")
	sameGrant := base
	sameGrant.Endpoint = "https://mail2.example/mcp"
	sameGrant.Authorization = oauthTestConfig("https://accounts.example/token")
	sameGrant.Authorization.Scopes = []string{"read", "write"}
	key := OAuthCredentialKey(base)
	require.NotEqual(t, key, OAuthCredentialKey(user))
	require.NotEqual(t, key, OAuthCredentialKey(otherClient))
	require.NotEqual(t, key, OAuthCredentialKey(otherTokenURL))
	require.Equal(t, key, OAuthCredentialKey(sameGrant))

	// A grant for the global definition never serves a same-named user definition or another client.
	provider := authorizationProvider(t)
	require.NoError(t, provider.store.Save(t.Context(), "alice", key, &oauth2.Token{AccessToken: "global-grant"}))
	_, err := provider.Resolve(t.Context(), "alice", base, nil)
	require.NoError(t, err)
	_, err = provider.Resolve(t.Context(), "alice", user, nil)
	require.ErrorIs(t, err, ErrCredentialNotFound)
	_, err = provider.Resolve(t.Context(), "alice", otherClient, nil)
	require.ErrorIs(t, err, ErrCredentialNotFound)

	// A user-owned definition only ever resolves for its owner.
	require.NoError(t, provider.store.Save(t.Context(), "bob", OAuthCredentialKey(user), &oauth2.Token{AccessToken: "bob-grant"}))
	_, err = provider.Resolve(t.Context(), "bob", user, nil)
	require.ErrorContains(t, err, "another namespace")
}
