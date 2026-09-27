package mcpclient

import (
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
}

// NewHandler serves GET /, PUT /{server}, DELETE /{server}, and optional OAuth
// GET /{server}/connect and GET /{server}/callback routes. Mount with http.StripPrefix.
// Writes are restricted to the authenticated namespace; globals are developer-owned.
// Without a namespace resolver requests fail closed. Pending OAuth state is process-local.
func NewHandler(store MCPServerConfigStore, options ...HandlerOption) http.Handler {
	// Share one catalog view and OAuth state map for the lifetime of this handler.
	settings := handlerOptions{}
	for _, option := range options {
		option(&settings)
	}
	h := &configHandler{store: store, client: NewClient(store), options: settings}
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

	// Return safe summaries rather than serializing configuration secrets to the browser.
	result := make([]ConnectorInfo, 0, len(resolved.configs)+len(resolved.invalid))
	for _, config := range resolved.configs {
		result = append(result, ConnectorInfo{
			Name: config.Name, Namespace: config.Namespace, Transport: config.Transport,
			ReadOnly: h.readOnly(config.Namespace, config.Name), OAuth: config.Authorization != nil,
		})
	}

	// Unusable records stay visible to their owner, who can delete or replace them.
	for name, err := range resolved.invalid {
		result = append(result, ConnectorInfo{
			Name: name, Namespace: namespace, ReadOnly: h.readOnly(namespace, name), Error: err.Error(),
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// readOnly reports whether the HTTP routes can change this definition.
func (h *configHandler) readOnly(namespace, name string) bool {
	if _, mutable := h.store.(MutableMCPServerConfigStore); namespace == "" || !mutable {
		return true
	}
	if store, ok := h.store.(*Store); ok {
		_, err := store.writable(namespace, name)
		return err != nil
	}
	return false
}

// writer protects globals regardless of the namespace specified in an incoming JSON body.
func (h *configHandler) writer(w http.ResponseWriter, r *http.Request) (string, MutableMCPServerConfigStore, bool) {
	namespace, resolved, ok := h.resolve(w, r)
	if !ok {
		return "", nil, false
	}
	if config, exists := resolved.configs[r.PathValue("server")]; exists && config.Namespace == "" {
		http.Error(w, "Global MCP configurations are read-only", http.StatusForbidden)
		return "", nil, false
	}
	store, ok := h.store.(MutableMCPServerConfigStore)
	if !ok {
		http.Error(w, "MCP configuration store is read-only", http.StatusForbidden)
		return "", nil, false
	}
	return namespace, store, true
}

func (h *configHandler) put(w http.ResponseWriter, r *http.Request) {
	namespace, store, ok := h.writer(w, r)
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
	namespace, store, ok := h.writer(w, r)
	if !ok {
		return
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
