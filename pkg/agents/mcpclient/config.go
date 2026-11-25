package mcpclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// ServerConfig is a serializable MCP definition. Namespace is its owner, which
// the Client sets from the store it read the definition from: empty for a
// built-in (developer-owned, global), the namespace for a user's own.
// Built-ins are always enabled; namespace-owned servers are enabled unless selected for disabling.
// Namespace-owned definitions must be literal (no templates), use public HTTP endpoints,
// and cannot set UseCredentials or CredentialsProvider, which hand application-held
// credentials to a global definition.
// Applications authorize writes; never let a user choose an arbitrary write namespace.
//
// A server's credentials come from at most one of: Authorization, an OAuth grant each
// user connects, through the client's CredentialProvider; UseCredentials, the client's
// CredentialProvider; CredentialsProvider, the server's own. Headers carry a static
// token; a server with none of them is open.
type ServerConfig struct {
	Name                  string            `json:"name"`
	Namespace             string            `json:"namespace,omitempty"`
	Endpoint              string            `json:"endpoint,omitempty"`
	Transport             string            `json:"transport,omitempty"`
	Command               []string          `json:"command,omitempty"`
	Env                   map[string]string `json:"env,omitempty"`
	Headers               map[string]string `json:"headers,omitempty"`
	Meta                  map[string]any    `json:"meta,omitempty"`
	ToolPrefix            string            `json:"toolPrefix,omitempty"`
	ToolFilter            ToolFilter        `json:"toolFilter,omitempty"`
	DeferredTools         *ToolFilter       `json:"deferredTools,omitempty"`
	ApprovalRequiredTools []string          `json:"approvalRequiredTools,omitempty"`
	CacheTTLSeconds       int64             `json:"cacheTTLSeconds,omitempty"`
	DefaultCacheScope     string            `json:"defaultCacheScope,omitempty"`
	DisableStandaloneSSE  bool              `json:"disableStandaloneSSE,omitempty"`
	UseCredentials        bool              `json:"useCredentials,omitempty"`
	Authorization         *OAuthConfig      `json:"authorization,omitempty"`

	// CredentialsProvider supplies this server's credentials in place of the
	// client's provider, as the MCP Go SDK's transports each take an OAuth
	// handler of their own. It is for definitions written in code, such as
	// NewInlineStore's: it is never JSON, so no stored definition carries one,
	// a store refuses to save one, and a user-owned definition cannot set one,
	// as its endpoint is the user's choice. A stdio server has no request to
	// put a token on.
	CredentialsProvider CredentialProvider `json:"-"`
}

// OAuthConfig is the JSON representation of a client's trusted OAuth application settings.
// Client secrets stay in application configuration, never in tool descriptors or run state.
//
// Only RedirectURL is required. Without AuthURL and TokenURL, the MCP authorization
// flow discovers the server's authorization server from its metadata (RFC 9728 and
// RFC 8414), and without a ClientID it also registers a client there (RFC 7591),
// keeping the registration in the credential store. AuthURL and TokenURL pin the
// endpoints instead, for servers that publish no metadata; they need a ClientID.
type OAuthConfig struct {
	ClientID       string            `json:"clientId"`
	ClientSecret   string            `json:"clientSecret,omitempty"`
	RedirectURL    string            `json:"redirectUrl"`
	AuthURL        string            `json:"authUrl"`
	TokenURL       string            `json:"tokenUrl"`
	Scopes         []string          `json:"scopes,omitempty"`
	AuthStyle      oauth2.AuthStyle  `json:"authStyle,omitempty"`
	AuthCodeParams map[string]string `json:"authCodeParams,omitempty"`
}

// cloneServerConfig isolates mutable configuration and verifies it can be stored as JSON.
// A definition's CredentialsProvider, which JSON never carries, comes along as it is.
func cloneServerConfig(config ServerConfig) (ServerConfig, error) {
	data, err := json.Marshal(config)
	if err != nil {
		return ServerConfig{}, errors.New("MCP server config must be JSON serializable")
	}
	var copy ServerConfig
	if err := json.Unmarshal(data, &copy); err != nil {
		return ServerConfig{}, err
	}
	copy.CredentialsProvider = config.CredentialsProvider
	return copy, nil
}

// validateServerConfig rejects ambiguous names and unsupported transports before storage or discovery.
func validateServerConfig(config ServerConfig) error {
	if config.Name == "" || config.Name != strings.TrimSpace(config.Name) || strings.ContainsAny(config.Name, "/\\") {
		return errors.New("MCP server name must be nonempty and contain no path separators")
	}
	if config.CacheTTLSeconds < 0 || config.CacheTTLSeconds > int64((1<<63-1)/time.Second) {
		return errors.New("invalid MCP cache TTL")
	}

	// Apply user restrictions to every source, including direct store writes and discovery.
	if config.Namespace != "" {
		if err := validateUserServerConfig(config); err != nil {
			return err
		}
	}

	switch config.Transport {
	case "", TransportSSE, TransportStreamableHTTP:
		if config.Endpoint == "" {
			return fmt.Errorf("MCP server %q requires an endpoint", config.Name)
		}
	case TransportStdio:
		if len(config.Command) == 0 {
			return fmt.Errorf("MCP server %q requires a command", config.Name)
		}
	default:
		return fmt.Errorf("MCP server %q has an unsupported transport", config.Name)
	}
	// Validate OAuth settings at the configuration boundary, including HTTP-only transport.
	if config.Authorization != nil {
		if config.Transport == TransportStdio {
			return errors.New("MCP OAuth requires an HTTP transport")
		}
		if _, err := resolveOAuthConfig(Connector{Name: config.Name, Namespace: config.Namespace, Endpoint: config.Endpoint, Authorization: config.Authorization}); err != nil {
			return err
		}
	}
	// A server takes its credentials from one source.
	sources := 0
	for _, set := range []bool{config.Authorization != nil, config.UseCredentials, config.CredentialsProvider != nil} {
		if set {
			sources++
		}
	}
	if sources > 1 {
		return fmt.Errorf("MCP server %q takes its credentials from one of authorization, useCredentials and its own credentials provider", config.Name)
	}
	if config.CredentialsProvider != nil && config.Transport == TransportStdio {
		return errors.New("an MCP server's credentials provider requires an HTTP transport")
	}
	if config.DefaultCacheScope != "" && config.DefaultCacheScope != CacheScopePublic && config.DefaultCacheScope != CacheScopePrivate {
		return errors.New("invalid default MCP cache scope")
	}

	return nil
}

// validateUserServerConfig keeps user definitions literal and restricted to public HTTP services.
func validateUserServerConfig(config ServerConfig) error {
	if config.Transport == TransportStdio || len(config.Command) != 0 || len(config.Env) != 0 {
		return errors.New("user MCP configurations must use an HTTP transport")
	}
	// The shared provider holds application credentials; a user-chosen endpoint must not receive them.
	// Users authenticate with literal headers or their own OAuth authorization instead.
	if config.UseCredentials {
		return errors.New("user MCP configurations cannot use the shared credential provider; use headers or authorization")
	}
	// A server's own provider holds application credentials too.
	if config.CredentialsProvider != nil {
		return errors.New("user MCP configurations cannot have a credentials provider; use headers or authorization")
	}

	// Inspect canonical JSON so nested metadata and typed maps receive the same checks.
	data, err := json.Marshal(config)
	if err != nil {
		return errors.New("MCP server config must be JSON serializable")
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	if containsTemplate(value) {
		return errors.New("user MCP configurations cannot contain templates")
	}

	// Literal private addresses fail at registration; DNS answers are checked at connection time.
	return validatePublicURL(config.Endpoint)
}

// containsTemplate rejects template delimiters, including escaped JSON and nested metadata keys.
func containsTemplate(value any) bool {
	switch value := value.(type) {
	case string:
		return strings.Contains(value, "{{")
	case map[string]any:
		for key, item := range value {
			if strings.Contains(key, "{{") || containsTemplate(item) {
				return true
			}
		}
	case []any:
		for _, item := range value {
			if containsTemplate(item) {
				return true
			}
		}
	}
	return false
}

// discovered reports whether the endpoints come from the server's metadata rather than this config.
func (c *OAuthConfig) discovered() bool {
	return c.AuthURL == "" && c.TokenURL == ""
}

// authorizationConfig creates the exchange options for one server definition.
func (c *OAuthConfig) authorizationConfig() *authorizationConfig {
	if c == nil {
		return nil
	}
	config := &authorizationConfig{OAuth: oauth2.Config{
		ClientID:     c.ClientID,
		ClientSecret: c.ClientSecret,
		RedirectURL:  c.RedirectURL,
		Scopes:       c.Scopes,
		Endpoint:     oauth2.Endpoint{AuthURL: c.AuthURL, TokenURL: c.TokenURL, AuthStyle: c.AuthStyle},
	}}

	// Sort optional consent parameters for deterministic authorization URLs.
	keys := make([]string, 0, len(c.AuthCodeParams))
	for key := range c.AuthCodeParams {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		config.AuthCodeOptions = append(config.AuthCodeOptions, oauth2.SetAuthURLParam(key, c.AuthCodeParams[key]))
	}
	config.AuthCodeParams = c.AuthCodeParams
	return config
}
