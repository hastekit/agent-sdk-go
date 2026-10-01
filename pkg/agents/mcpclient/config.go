package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// ServerConfig is a serializable MCP definition. Empty Namespace means developer-owned/global.
// Stores must return only global definitions and definitions owned by the requested namespace.
// Globals are always enabled; namespace-owned servers are enabled unless selected for disabling.
// Namespace-owned definitions must be literal (no templates), use public HTTP endpoints,
// and cannot set UseCredentials, which hands the client's CredentialProvider to a global definition.
// Applications authorize writes; never let a user choose an arbitrary write namespace.
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

// MCPServerConfigStore lists all definitions visible to a subject in one consistent snapshot.
// A database adapter selects globals and records whose namespace matches the subject.
// Listing with an empty namespace returns global definitions only.
type MCPServerConfigStore interface {
	ListServerConfigs(ctx context.Context, namespace string, runContext map[string]any) ([]ServerConfig, error)
}

// cloneServerConfig isolates mutable configuration and verifies it can be stored as JSON.
func cloneServerConfig(config ServerConfig) (ServerConfig, error) {
	data, err := json.Marshal(config)
	if err != nil {
		return ServerConfig{}, errors.New("MCP server config must be JSON serializable")
	}
	var copy ServerConfig
	err = json.Unmarshal(data, &copy)
	return copy, err
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
