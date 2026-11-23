package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
)

// HandlerOption configures trusted identity resolution and optional OAuth routes.
type HandlerOption func(*handlerOptions)

type handlerOptions struct {
	namespace func(*http.Request) (string, error)
	oauth     *OAuthCredentialProvider
	readOnly  []MCPServerConfigStore
}

// WithNamespaceResolver supplies the authenticated subject used by agent runs.
// Never derive this identity from an untrusted query parameter or request body.
func WithNamespaceResolver(resolve func(*http.Request) (string, error)) HandlerOption {
	return func(options *handlerOptions) { options.namespace = resolve }
}

// WithOAuth enables authorization routes using the same provider as the MCP client.
func WithOAuth(provider *OAuthCredentialProvider) HandlerOption {
	return func(options *handlerOptions) { options.oauth = provider }
}

// WithReadOnlyStores adds stores the handler lists but never writes, such as an
// InlineStore of servers defined in code. Pass every store the agents' clients
// read besides the writable one, so users see those servers, can connect their
// OAuth accounts to them, and cannot take a global server's name.
func WithReadOnlyStores(stores ...MCPServerConfigStore) HandlerOption {
	return func(options *handlerOptions) { options.readOnly = append(options.readOnly, stores...) }
}

// ConnectorInfo is the public catalog entry returned by the HTTP handler.
// Secrets, headers, environment variables, and OAuth application settings are never returned.
type ConnectorInfo struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Transport string `json:"transport,omitempty"`
	ReadOnly  bool   `json:"readOnly"`
	OAuth     bool   `json:"oauth"`
	// Error explains why a user-owned definition is unusable; delete or replace it to recover.
	Error string `json:"error,omitempty"`
	// Connected reports whether the requester has authorized an OAuth server. It is
	// set only when the handler serves OAuth routes, so a client can offer connect.
	Connected *bool `json:"connected,omitempty"`
}

// NewHandler serves GET /, PUT /{server}, DELETE /{server}, and optional OAuth
// GET /{server}/connect and GET /{server}/callback routes. Mount with http.StripPrefix.
// Writes go to store and are restricted to the authenticated namespace; globals
// are developer-owned. WithReadOnlyStores adds stores that are listed but never
// written. Without a namespace resolver requests fail closed. Pending OAuth state
// is process-local.
func NewHandler(store MCPServerConfigStore, options ...HandlerOption) http.Handler {
	// Share one catalog view and OAuth state map for the lifetime of this handler.
	settings := handlerOptions{}
	for _, option := range options {
		option(&settings)
	}
	stores := append([]MCPServerConfigStore{store}, settings.readOnly...)
	h := &configHandler{store: store, client: NewClient(stores...), options: settings}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", h.list)
	mux.HandleFunc("PUT /{server}", h.put)
	mux.HandleFunc("DELETE /{server}", h.remove)
	if settings.oauth != nil {
		oauth := &oauthHandler{provider: settings.oauth, client: h.client, namespace: h.namespace, pending: make(map[string]pendingAuthorization)}
		mux.HandleFunc("GET /{server}/connect", oauth.connect)
		mux.HandleFunc("GET /{server}/callback", oauth.callback)
	}
	return mux
}

type configHandler struct {
	store   MCPServerConfigStore
	client  *Client
	options handlerOptions
}

func (h *configHandler) namespace(r *http.Request) (string, error) {
	if h.options.namespace == nil {
		return "", errors.New("MCP handler requires an authenticated namespace resolver")
	}
	return h.options.namespace(r)
}

// resolve scopes every catalog read and write to authenticated application identity.
func (h *configHandler) resolve(w http.ResponseWriter, r *http.Request) (string, catalog, bool) {
	w.Header().Set("Cache-Control", "no-store")
	namespace, err := h.namespace(r)
	if err != nil || strings.TrimSpace(namespace) == "" {
		http.Error(w, "Unable to resolve authenticated namespace", http.StatusForbidden)
		return "", catalog{}, false
	}
	resolved, err := h.client.listServerConfigs(r.Context(), namespace, nil)
	if err != nil {
		http.Error(w, "Unable to read MCP configuration", http.StatusInternalServerError)
		return "", catalog{}, false
	}
	return namespace, resolved, true
}

func (h *configHandler) list(w http.ResponseWriter, r *http.Request) {
	namespace, resolved, ok := h.resolve(w, r)
	if !ok {
		return
	}

	owned, err := h.owned(r.Context(), namespace)
	if err != nil {
		http.Error(w, "Unable to read MCP configuration", http.StatusInternalServerError)
		return
	}

	// Return safe summaries rather than serializing configuration secrets to the browser.
	result := make([]ConnectorInfo, 0, len(resolved.configs)+len(resolved.invalid))
	for _, config := range resolved.configs {
		info := ConnectorInfo{
			Name: config.Name, Namespace: config.Namespace, Transport: config.Transport,
			ReadOnly: config.Namespace == "" || !owned[config.Name], OAuth: config.Authorization != nil,
		}
		if h.options.oauth != nil && config.Authorization != nil {
			_, err := h.options.oauth.store.Load(r.Context(), namespace, OAuthCredentialKey(connectorFor(config)))
			connected := err == nil
			info.Connected = &connected
		}
		result = append(result, info)
	}

	// Unusable records stay visible to their owner, who can delete or replace them.
	for name, err := range resolved.invalid {
		result = append(result, ConnectorInfo{
			Name: name, Namespace: namespace, ReadOnly: !owned[name], Error: err.Error(),
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// owned names the namespace's definitions the writable store holds. Only these
// can change through the HTTP routes: a user's definition from a read-only
// store, or behind a read-only Store, is listed but not editable.
func (h *configHandler) owned(ctx context.Context, namespace string) (map[string]bool, error) {
	owned := map[string]bool{}
	if store, ok := h.store.(*Store); ok {
		if _, err := store.writable(); err != nil {
			return owned, nil
		}
	} else if _, mutable := h.store.(MutableMCPServerConfigStore); !mutable {
		return owned, nil
	}
	configs, err := h.store.ListServerConfigs(ctx, namespace, nil)
	if err != nil {
		return nil, err
	}
	for _, config := range configs {
		if config.Namespace == namespace {
			owned[config.Name] = true
		}
	}
	return owned, nil
}

// writer protects globals regardless of the namespace specified in an incoming JSON body.
func (h *configHandler) writer(w http.ResponseWriter, r *http.Request) (string, catalog, MutableMCPServerConfigStore, bool) {
	namespace, resolved, ok := h.resolve(w, r)
	if !ok {
		return "", catalog{}, nil, false
	}
	name := r.PathValue("server")
	if config, exists := resolved.configs[name]; exists && config.Namespace == "" {
		http.Error(w, "Global MCP configurations are read-only", http.StatusForbidden)
		return "", catalog{}, nil, false
	}
	store, ok := h.store.(MutableMCPServerConfigStore)
	if !ok {
		http.Error(w, "MCP configuration store is read-only", http.StatusForbidden)
		return "", catalog{}, nil, false
	}

	// A visible definition the writable store does not hold belongs to a read-only store.
	_, valid := resolved.configs[name]
	_, invalid := resolved.invalid[name]
	if valid || invalid {
		owned, err := h.owned(r.Context(), namespace)
		if err != nil {
			http.Error(w, "Unable to read MCP configuration", http.StatusInternalServerError)
			return "", catalog{}, nil, false
		}
		if !owned[name] {
			http.Error(w, "MCP configuration is read-only", http.StatusForbidden)
			return "", catalog{}, nil, false
		}
	}
	return namespace, resolved, store, true
}

func (h *configHandler) put(w http.ResponseWriter, r *http.Request) {
	namespace, _, store, ok := h.writer(w, r)
	if !ok {
		return
	}

	// Accept one bounded JSON definition; identity and name come from the trusted route context.
	var config ServerConfig
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		http.Error(w, "Invalid MCP configuration", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		http.Error(w, "Expected one MCP configuration", http.StatusBadRequest)
		return
	}
	config.Name = r.PathValue("server")
	config.Namespace = namespace
	if err := validateServerConfig(config); err != nil {
		http.Error(w, "Invalid MCP configuration", http.StatusBadRequest)
		return
	}
	if err := store.Put(r.Context(), namespace, config); err != nil {
		configWriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *configHandler) remove(w http.ResponseWriter, r *http.Request) {
	namespace, resolved, store, ok := h.writer(w, r)
	if !ok {
		return
	}

	// Forget the user's grant before the definition, so a failure never leaves an orphaned token.
	if config, exists := resolved.configs[r.PathValue("server")]; exists && config.Authorization != nil && h.options.oauth != nil {
		key := OAuthCredentialKey(connectorFor(config))
		lock := h.options.oauth.credentialLock(namespace, key)
		lock.Lock()
		err := h.options.oauth.store.Delete(r.Context(), namespace, key)
		// A client registered for a discovered server goes with it.
		if clients, ok := h.options.oauth.store.(OAuthClientStore); ok && err == nil {
			err = clients.DeleteOAuthClient(r.Context(), namespace, key)
		}
		lock.Unlock()
		if err != nil {
			http.Error(w, "Unable to remove MCP authorization", http.StatusInternalServerError)
			return
		}
	}
	if err := store.Delete(r.Context(), namespace, r.PathValue("server")); err != nil {
		configWriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func configWriteError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrReadOnlyConfig) {
		http.Error(w, "MCP configuration is read-only", http.StatusForbidden)
		return
	}
	http.Error(w, "Unable to persist MCP configuration", http.StatusInternalServerError)
}

// connectorFor names the definition the way credential providers see it.
func connectorFor(config ServerConfig) Connector {
	return Connector{Namespace: config.Namespace, Name: config.Name, Endpoint: config.Endpoint, Authorization: config.Authorization}
}
