package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

// providerTestResource is an MCP server at /mcp serving one tool to requests
// bearing a token starting wantBearer, or to every request when wantBearer is
// empty, and refusing others. It records every Authorization header it is sent.
func providerTestResource(t *testing.T, tool, wantBearer string, bearers *sync.Map) *httptest.Server {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: tool, Version: "0.1.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: tool},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
		})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if bearers != nil && auth != "" {
			bearers.Store(auth, true)
		}
		if wantBearer != "" && !strings.HasPrefix(auth, "Bearer "+wantBearer) {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
	return srv
}

// sent are the Authorization headers bearers recorded.
func sent(bearers *sync.Map) []string {
	var headers []string
	bearers.Range(func(key, _ any) bool {
		headers = append(headers, key.(string))
		return true
	})
	return headers
}

func fileTestStore(t *testing.T) *FileCredentialStore {
	t.Helper()
	store, err := NewFileCredentialStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

// hostTokens is a credentials provider as a host writes one: it gives each
// user's token for its server from wherever the host keeps them, here a map a
// sign-in fills, and an authentication status to a user who has none.
type hostTokens struct {
	mu     sync.Mutex
	tokens map[string]string // namespace → token
	asked  atomic.Int32
}

func (p *hostTokens) signIn(namespace, token string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tokens == nil {
		p.tokens = map[string]string{}
	}
	p.tokens[namespace] = token
}

func (p *hostTokens) Resolve(_ context.Context, namespace string, _ Connector, _ map[string]any) (*Credential, error) {
	p.asked.Add(1)
	p.mu.Lock()
	token, ok := p.tokens[namespace]
	p.mu.Unlock()
	if !ok {
		return nil, agents.NewToolsetError(agents.ToolsetErrorAuth, errors.New("not signed in"))
	}
	return &Credential{TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token}), Principal: namespace}, nil
}

// A server takes its credentials from one source; its own credentials provider
// is for global servers over HTTP only, as a user-owned one's endpoint is the
// user's choice.
func TestCredentialsProviderValidation(t *testing.T) {
	provider := &hostTokens{}
	valid := func() ServerConfig {
		return ServerConfig{Name: "planner", Endpoint: "https://mcp.example/planner/mcp", CredentialsProvider: provider}
	}
	require.NoError(t, validateServerConfig(valid()))
	for name, edit := range map[string]func(*ServerConfig){
		"user-owned":         func(c *ServerConfig) { c.Namespace = "alice" },
		"with authorization": func(c *ServerConfig) { c.Authorization = &OAuthConfig{RedirectURL: "http://localhost/callback"} },
		"with credentials":   func(c *ServerConfig) { c.UseCredentials = true },
		"over stdio":         func(c *ServerConfig) { c.Endpoint, c.Transport, c.Command = "", TransportStdio, []string{"planner"} },
		"authorization and credentials": func(c *ServerConfig) {
			c.CredentialsProvider, c.UseCredentials, c.Authorization = nil, true, &OAuthConfig{RedirectURL: "http://localhost/callback"}
		},
	} {
		config := valid()
		edit(&config)
		require.Error(t, validateServerConfig(config), name)
	}
}

// A credentials provider is code's alone: a definition written in code keeps it
// through the client's listing, while no JSON carries one, no store saves one,
// and a user store's record that sets one is unusable.
func TestCredentialsProviderIsCodeOnly(t *testing.T) {
	provider := &hostTokens{}
	config := ServerConfig{Name: "planner", Endpoint: "https://mcp.example/planner/mcp", CredentialsProvider: provider}

	resolved, err := NewClient(NewInlineStore(config)).listServerConfigs(t.Context(), "alice", nil)
	require.NoError(t, err)
	require.NotNil(t, resolved.configs["planner"].CredentialsProvider, "a definition in code keeps its provider")

	data, err := json.Marshal(config)
	require.NoError(t, err)
	assert.NotContains(t, strings.ToLower(string(data)), "provider")
	var decoded ServerConfig
	require.NoError(t, json.Unmarshal([]byte(`{"name":"planner","endpoint":"https://mcp.example/planner/mcp","credentialsProvider":{"x":1},"CredentialsProvider":"y"}`), &decoded))
	assert.Nil(t, decoded.CredentialsProvider, "no JSON sets one")

	files, err := NewFileStore(t.TempDir() + "/mcp.json")
	require.NoError(t, err)
	for name, store := range map[string]MutableStore{"memory": NewMemoryStore(), "file": files} {
		require.Error(t, store.Put(t.Context(), "", config), name)
		require.Error(t, store.Put(t.Context(), "alice", config), name)
	}

	users := &recordStore{records: []ServerConfig{{Name: "mine", Namespace: "alice", Endpoint: "https://mcp.example/mine", CredentialsProvider: provider}}}
	resolved, err = NewClient(nil, WithUserStore(users)).listServerConfigs(t.Context(), "alice", nil)
	require.NoError(t, err)
	assert.Error(t, resolved.invalid["mine"], "a user's server never has a provider of its own")
	assert.NotContains(t, resolved.configs, "mine")
}

// A server's own provider gives its tokens; the client's provider serves the
// servers that ask for it, and is never asked about this one.
func TestCredentialsProviderIsUsedForItsServer(t *testing.T) {
	var bearers sync.Map
	own := providerTestResource(t, "list_programs", "own-token", &bearers)
	shared := providerTestResource(t, "list_reports", "shared-token", &bearers)
	var ownAsked, sharedAsked atomic.Int32
	ownProvider := CredentialProviderFn(func(_ context.Context, namespace string, connector Connector, _ map[string]any) (*Credential, error) {
		ownAsked.Add(1)
		assert.Equal(t, "planner", connector.Name)
		return &Credential{TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "own-token"}), Principal: namespace}, nil
	})
	sharedProvider := CredentialProviderFn(func(_ context.Context, namespace string, connector Connector, _ map[string]any) (*Credential, error) {
		sharedAsked.Add(1)
		assert.Equal(t, "reports", connector.Name)
		return &Credential{TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "shared-token"}), Principal: namespace}, nil
	})
	client := NewClient(NewInlineStore(
		ServerConfig{Name: "planner", Endpoint: own.URL + "/mcp", Transport: TransportStreamableHTTP, CredentialsProvider: ownProvider},
		ServerConfig{Name: "reports", Endpoint: shared.URL + "/mcp", Transport: TransportStreamableHTTP, UseCredentials: true},
	), WithCredentials(sharedProvider))

	statuses, tools, err := client.ListTools(t.Context(), "alice", nil)
	require.NoError(t, err)
	require.Len(t, statuses, 2)
	for _, status := range statuses {
		assert.True(t, status.Connected, status.Name, status.Detail)
	}
	assert.Len(t, tools, 2)
	assert.Positive(t, ownAsked.Load())
	assert.Positive(t, sharedAsked.Load())
	_, crossed := bearers.Load("Bearer shared-token")
	assert.True(t, crossed, "the shared provider's server got its token")

	// A server with a provider of its own needs no client provider.
	alone := NewClient(NewInlineStore(ServerConfig{Name: "planner", Endpoint: own.URL + "/mcp", Transport: TransportStreamableHTTP, CredentialsProvider: ownProvider}))
	statuses, _, err = alone.ListTools(t.Context(), "alice", nil)
	require.NoError(t, err)
	assert.True(t, statuses[0].Connected, statuses[0].Detail)
}

// Connections are keyed by the provider whose tokens they carry, so a server
// rebuilt with another provider never gets a session the first one opened.
func TestPoolKeyNamesTheProvider(t *testing.T) {
	first, second := &hostTokens{}, &hostTokens{}
	fn := CredentialProviderFn(func(context.Context, string, Connector, map[string]any) (*Credential, error) { return nil, nil })
	assert.Empty(t, authorizationDigest(nil, nil))
	assert.Equal(t, authorizationDigest(nil, first), authorizationDigest(nil, first))
	assert.NotEqual(t, authorizationDigest(nil, first), authorizationDigest(nil, second))
	assert.NotEqual(t, authorizationDigest(nil, first), authorizationDigest(nil, fn))
}

// One client mixes servers whose users connect them (OAuth, through the
// client's provider), whose tokens a provider of the host's own gives, that
// take a static token, and that are open. Each gets its own credentials, the
// host's once its user signed in there, and no server's token reaches another.
func TestClientMixesCredentialStrategies(t *testing.T) {
	var hostBearers, staticBearers, openBearers sync.Map
	hosted := providerTestResource(t, "list_programs", "host-", &hostBearers)
	static := providerTestResource(t, "list_reports", "static-token", &staticBearers)
	open := providerTestResource(t, "list_assets", "", &openBearers)
	host := &hostTokens{}
	oauth, err := NewOAuthCredentialProvider(OAuthCredentialProviderConfig{Store: fileTestStore(t)})
	require.NoError(t, err)
	client := NewClient(NewInlineStore(
		ServerConfig{Name: "planner", Endpoint: hosted.URL + "/mcp", Transport: TransportStreamableHTTP, CredentialsProvider: host},
		ServerConfig{Name: "reports", Endpoint: static.URL + "/mcp", Transport: TransportStreamableHTTP, Headers: map[string]string{"Authorization": "Bearer static-token"}},
		ServerConfig{Name: "assets", Endpoint: open.URL + "/mcp", Transport: TransportStreamableHTTP},
		ServerConfig{Name: "gmail", Endpoint: "https://gmail.example/mcp", Transport: TransportStreamableHTTP, Authorization: oauthTestConfig("https://accounts.example/token")},
	), WithCredentials(oauth))
	ctx := context.Background()
	statuses := func() map[string]agents.ConnectorStatus {
		t.Helper()
		listed, _, err := client.ListTools(ctx, "alice", nil)
		require.NoError(t, err)
		byName := map[string]agents.ConnectorStatus{}
		for _, status := range listed {
			byName[status.Name] = status
		}
		return byName
	}

	before := statuses()
	assert.False(t, before["planner"].Connected)
	assert.Equal(t, agents.ToolsetErrorAuth, before["planner"].Kind)
	assert.True(t, before["reports"].Connected, before["reports"].Detail)
	assert.True(t, before["assets"].Connected, before["assets"].Detail)
	assert.False(t, before["gmail"].Connected)
	assert.Equal(t, agents.ToolsetErrorAuth, before["gmail"].Kind)

	host.signIn("alice", "host-alice")
	after := statuses()
	assert.True(t, after["planner"].Connected, after["planner"].Detail)
	assert.Equal(t, 1, after["planner"].ToolCount)
	assert.False(t, after["gmail"].Connected, "an OAuth server still waits for its user to connect it")
	assert.ElementsMatch(t, []string{"Bearer host-alice"}, sent(&hostBearers))
	assert.ElementsMatch(t, []string{"Bearer static-token"}, sent(&staticBearers))
	assert.Empty(t, sent(&openBearers), "an open server is sent no credentials")
}

// updatingStore is a credential store updating records through
// UpdateCredential, as a database store would under a row lock.
type updatingStore struct {
	*FileCredentialStore
	mu      sync.Mutex
	updates atomic.Int32
}

func (s *updatingStore) UpdateCredential(ctx context.Context, namespace, key string, update func(*oauth2.Token) (*oauth2.Token, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updates.Add(1)
	current, err := s.Load(ctx, namespace, key)
	if errors.Is(err, ErrCredentialNotFound) {
		current, err = nil, nil
	}
	if err != nil {
		return err
	}
	next, err := update(current)
	switch {
	case err != nil:
		return err
	case next == nil && current != nil:
		return s.Delete(ctx, namespace, key)
	case next != nil && next != current:
		return s.Save(ctx, namespace, key, next)
	}
	return nil
}

// OAuth grants users connect refresh through a CredentialUpdater.
func TestOAuthRefreshesThroughCredentialUpdater(t *testing.T) {
	var refreshes atomic.Int32
	accounts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if r.PostForm.Get("grant_type") != "refresh_token" || r.PostForm.Get("refresh_token") != "old-refresh" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		n := refreshes.Add(1)
		_, _ = fmt.Fprintf(w, `{"access_token":"refreshed-%d","token_type":"bearer","expires_in":3600,"refresh_token":"rotated-%d"}`, n, n)
	}))
	t.Cleanup(accounts.Close)
	store := &updatingStore{FileCredentialStore: fileTestStore(t)}
	provider, err := NewOAuthCredentialProvider(OAuthCredentialProviderConfig{Store: store})
	require.NoError(t, err)
	connector := Connector{Name: "gmail", Endpoint: "https://gmail.example/mcp", Authorization: oauthTestConfig(accounts.URL + "/token")}
	ctx := context.Background()
	key := OAuthCredentialKey(connector)
	require.NoError(t, store.Save(ctx, "alice", key, &oauth2.Token{AccessToken: "old", RefreshToken: "old-refresh", Expiry: time.Now().Add(-time.Minute)}))

	cred, err := provider.Resolve(ctx, "alice", connector, nil)
	require.NoError(t, err)
	token, err := cred.TokenSource.Token()
	require.NoError(t, err)
	assert.Equal(t, "refreshed-1", token.AccessToken)
	assert.Equal(t, int32(1), store.updates.Load())
	stored, err := store.Load(ctx, "alice", key)
	require.NoError(t, err)
	assert.Equal(t, "rotated-1", stored.RefreshToken)

	// A fresh token is sent without locking the record.
	_, err = cred.TokenSource.Token()
	require.NoError(t, err)
	assert.Equal(t, int32(1), store.updates.Load())
}
