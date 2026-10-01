package mcpclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"golang.org/x/oauth2"
)

// authorizationConfig contains the OAuth exchange settings resolved from a server config.
type authorizationConfig struct {
	OAuth           oauth2.Config
	AuthCodeOptions []oauth2.AuthCodeOption
	// AuthCodeParams are AuthCodeOptions as written, for an authorization URL
	// built by the MCP SDK rather than by OAuth.
	AuthCodeParams map[string]string
}

// oauthServerConfig is a snapshot used by one authorization attempt or token source.
type oauthServerConfig struct {
	Endpoint  string
	Namespace string
	// Discovered marks endpoints found through the server's metadata, and a
	// client registered there when the config names none; both are kept in
	// the credential store (see OAuthClientStore) once the user authorizes.
	Discovered bool
	// Key names the stored grant, as OAuthCredentialKey does for the config.
	Key string
	authorizationConfig
}

// OAuthCredentialProviderConfig configures shared persistence and token exchange settings.
// OAuth application settings come from each ServerConfig, not a provider registry.
type OAuthCredentialProviderConfig struct {
	Store CredentialStore

	// HTTPClient carries trusted transport settings for global server token exchanges.
	// User-owned servers always use the SDK's public-network transport without proxies.
	HTTPClient *http.Client
	// TokenTimeout defaults to 30 seconds and bounds each exchange or refresh.
	TokenTimeout time.Duration
}

// OAuthCredentialProvider resolves tokens by namespace and server, persisting every refresh.
// Share one provider per process. Multiple processes require a store/refresh coordinator
// designed for concurrent refresh-token rotation; the file store is intended for local use.
type OAuthCredentialProvider struct {
	store   CredentialStore
	client  *http.Client
	timeout time.Duration
	locks   sync.Map
}

var _ CredentialProvider = (*OAuthCredentialProvider)(nil)

// NewOAuthCredentialProvider creates shared token persistence and refresh coordination.
func NewOAuthCredentialProvider(cfg OAuthCredentialProviderConfig) (*OAuthCredentialProvider, error) {
	// Credentials must never fall back to an anonymous connection due to missing persistence.
	if cfg.Store == nil {
		return nil, errors.New("MCP OAuth requires a credential store")
	}
	if cfg.TokenTimeout <= 0 {
		cfg.TokenTimeout = 30 * time.Second
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}

	// Configuration is supplied with each resolution; the provider owns only credentials.
	return &OAuthCredentialProvider{
		store: cfg.Store, client: cfg.HTTPClient, timeout: cfg.TokenTimeout,
	}, nil
}

// OAuthCredentialKey names the stored grant for one connector definition; it is the
// key OAuthCredentialProvider passes to CredentialStore alongside the namespace.
// It binds the definition's owner (global or the user's own), its name, and the
// OAuth client and token endpoint that issued the grant. A token therefore never
// reaches a different definition that reuses the name, and a refresh token is never
// sent through a different client or token endpoint: changing either requires the
// user to reconnect. Pass it to CredentialStore.Delete to disconnect an account.
//
// A definition whose endpoints are discovered is bound to its MCP endpoint
// instead of a token endpoint it does not name: the endpoint decides which
// authorization server, and which registered client, issue its grants.
func OAuthCredentialKey(connector Connector) string {
	var clientID, tokenURL string
	discovered := false
	if connector.Authorization != nil {
		clientID, tokenURL = connector.Authorization.ClientID, connector.Authorization.TokenURL
		discovered = connector.Authorization.discovered()
	}
	return oauthCredentialKey(connector.Namespace != "", connector.Name, clientID, tokenURL, discovered, connector.Endpoint)
}

// oauthCredentialKey is unambiguous because server names cannot contain path separators.
func oauthCredentialKey(userOwned bool, name, clientID, tokenURL string, discovered bool, endpoint string) string {
	scope := "global"
	if userOwned {
		scope = "user"
	}
	data, _ := json.Marshal([2]string{clientID, tokenURL})
	if discovered {
		data, _ = json.Marshal([3]string{"discovered", clientID, endpoint})
	}
	digest := sha256.Sum256(data)
	return scope + "/" + name + "/" + hex.EncodeToString(digest[:8])
}

// resolveOAuthConfig validates the current store definition before exchanging credentials.
func resolveOAuthConfig(connector Connector) (oauthServerConfig, error) {
	authorization := connector.Authorization
	if authorization == nil {
		return oauthServerConfig{}, errors.New("MCP OAuth requires authorization config")
	}
	discovered := authorization.discovered()
	switch {
	case !discovered && (authorization.AuthURL == "" || authorization.TokenURL == ""):
		return oauthServerConfig{}, errors.New("MCP OAuth needs both an authorization URL and a token URL, or neither to discover them")
	case !discovered && strings.TrimSpace(authorization.ClientID) == "":
		return oauthServerConfig{}, errors.New("MCP OAuth with its own endpoints requires a client ID")
	case authorization.ClientSecret != "" && strings.TrimSpace(authorization.ClientID) == "":
		return oauthServerConfig{}, errors.New("MCP OAuth client secret requires a client ID")
	}
	config := authorization.authorizationConfig()
	endpoints := []string{connector.Endpoint, config.OAuth.RedirectURL}
	if !discovered {
		endpoints = append(endpoints, config.OAuth.Endpoint.AuthURL, config.OAuth.Endpoint.TokenURL)
	}
	for _, endpoint := range endpoints {
		if !validOAuthURL(endpoint) {
			return oauthServerConfig{}, fmt.Errorf("MCP OAuth server %q requires HTTPS URLs (HTTP is allowed for loopback only)", connector.Name)
		}
	}

	// Only the endpoint and token URL are requested by this process, so only they can reach
	// internal services. The auth and redirect URLs are browser navigations, which a loopback
	// callback needs during local development.
	// Discovered endpoints are checked as they are requested, by the same transport.
	if connector.Namespace != "" {
		for _, endpoint := range []string{connector.Endpoint, config.OAuth.Endpoint.TokenURL} {
			if endpoint == "" {
				continue
			}
			if err := validatePublicURL(endpoint); err != nil {
				return oauthServerConfig{}, err
			}
		}
	}
	return oauthServerConfig{
		Endpoint:            connector.Endpoint,
		Namespace:           connector.Namespace,
		Discovered:          discovered,
		Key:                 OAuthCredentialKey(connector),
		authorizationConfig: *config,
	}, nil
}

// validOAuthURL permits local development without permitting credentials over remote plaintext HTTP.
func validOAuthURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	return u.Scheme == "https" || u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")
}

// Resolve uses the namespace supplied by the agent, keeping tokens out of serializable run context.
// Namespace is explicit and is never read from the free-form run context.
func (p *OAuthCredentialProvider) Resolve(ctx context.Context, namespace string, connector Connector, runContext map[string]any) (*Credential, error) {
	// The client or handler supplies settings from the authoritative config store.
	if p == nil {
		return nil, errors.New("MCP OAuth credential provider is nil")
	}
	server, err := resolveOAuthConfig(connector)
	if err != nil {
		return nil, err
	}
	// A user-owned definition only ever serves its owner; stores must not cross namespaces.
	if connector.Namespace != "" && connector.Namespace != namespace {
		return nil, agents.NewToolsetError(agents.ToolsetErrorAuth, errors.New("MCP server belongs to another namespace"))
	}
	key := server.Key
	if _, err := credentialKey(namespace, key); err != nil {
		return nil, agents.NewToolsetError(agents.ToolsetErrorAuth, err)
	}

	// Detect disconnected accounts before opening a session or serving a cached listing.
	if _, err := p.store.Load(ctx, namespace, key); err != nil {
		if errors.Is(err, ErrCredentialNotFound) {
			return nil, agents.NewToolsetError(agents.ToolsetErrorAuth, err)
		}
		return nil, err
	}
	if server, err = p.withClient(ctx, namespace, server); err != nil {
		if errors.Is(err, ErrCredentialNotFound) {
			return nil, agents.NewToolsetError(agents.ToolsetErrorAuth, err)
		}
		return nil, err
	}
	return &Credential{
		Principal:   namespace,
		TokenSource: &persistedTokenSource{provider: p, namespace: namespace, key: key, config: server},
	}, nil
}

// oauthClients is the credential store's OAuthClientStore, which servers with
// discovered endpoints need.
func (p *OAuthCredentialProvider) oauthClients() (OAuthClientStore, error) {
	clients, ok := p.store.(OAuthClientStore)
	if !ok {
		return nil, errors.New("MCP OAuth with discovered endpoints needs a credential store that implements OAuthClientStore")
	}
	return clients, nil
}

// withClient completes a discovered server's snapshot with the client and
// endpoints its grant was issued through, as the authorization stored them.
func (p *OAuthCredentialProvider) withClient(ctx context.Context, namespace string, server oauthServerConfig) (oauthServerConfig, error) {
	if !server.Discovered {
		return server, nil
	}
	clients, err := p.oauthClients()
	if err != nil {
		return server, err
	}
	client, err := clients.LoadOAuthClient(ctx, namespace, server.Key)
	if err != nil {
		return server, err
	}
	server.OAuth.ClientID, server.OAuth.ClientSecret = client.ClientID, client.ClientSecret
	server.OAuth.Endpoint = oauth2.Endpoint{AuthURL: client.AuthURL, TokenURL: client.TokenURL, AuthStyle: client.AuthStyle}
	server.OAuth.Scopes = client.Scopes
	return server, nil
}

// credentialLock serializes refreshes and authorization writes for one stored grant in this provider.
func (p *OAuthCredentialProvider) credentialLock(namespace, key string) *sync.Mutex {
	id, _ := credentialKey(namespace, key)
	lock, _ := p.locks.LoadOrStore(id, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// httpClient applies a config's ownership policy to every OAuth request made for
// it: discovery, registration, code exchange and token refresh.
func (p *OAuthCredentialProvider) httpClient(config oauthServerConfig) *http.Client {
	if config.Namespace != "" {
		return &http.Client{Transport: publicRoundTripper{base: userHTTPTransport}, CheckRedirect: sameOriginRedirect}
	}
	return p.client
}

// tokenContext outlives individual run contexts because pooled MCP connections can outlive a run.
func (p *OAuthCredentialProvider) tokenContext(parent context.Context, config oauthServerConfig) (context.Context, context.CancelFunc) {
	ctx := context.WithValue(parent, oauth2.HTTPClient, p.httpClient(config))
	return context.WithTimeout(ctx, p.timeout)
}

// persistedTokenSource reads the latest stored token on each request, including after reconnects.
type persistedTokenSource struct {
	provider  *OAuthCredentialProvider
	namespace string
	key       string
	config    oauthServerConfig
}

// Token refreshes only expired tokens and saves any rotated refresh token before returning it.
func (s *persistedTokenSource) Token() (*oauth2.Token, error) {
	// Coordinate all sources for this subject, including separate pooled MCP connections.
	p := s.provider
	lock := p.credentialLock(s.namespace, s.key)
	lock.Lock()
	defer lock.Unlock()
	ctx, cancel := p.tokenContext(context.Background(), s.config)
	defer cancel()

	// Reload inside the lock so another request's refresh is immediately visible.
	token, err := p.store.Load(ctx, s.namespace, s.key)
	if err != nil {
		return nil, err
	}
	if token.Valid() {
		return token, nil
	}
	if token.RefreshToken == "" {
		return nil, errors.New("MCP OAuth token expired; reconnect this account")
	}

	// Never propagate OAuth response bodies: providers may echo sensitive values in them.
	config := s.config.OAuth
	refreshed, err := config.TokenSource(ctx, token).Token()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("MCP OAuth refresh failed; reconnect this account")
	}
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = token.RefreshToken
	}
	if err := p.store.Save(ctx, s.namespace, s.key, refreshed); err != nil {
		return nil, fmt.Errorf("persist refreshed MCP credential: %w", err)
	}
	return refreshed, nil
}
