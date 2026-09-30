package mcpclient

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

const oauthStateLifetime = 10 * time.Minute

// pendingAuthorization binds an expiring authorization attempt to its subject, server and PKCE verifier.
// key names the grant for the captured config, so the callback saves under the client that issued it.
type pendingAuthorization struct {
	namespace, server, key, verifier string
	expires                          time.Time
	config                           oauthServerConfig
}

// oauthHandler keeps only short-lived authorization attempts in memory; credentials use the store.
type oauthHandler struct {
	provider  *OAuthCredentialProvider
	client    *Client
	namespace func(*http.Request) (string, error)
	mu        sync.Mutex
	pending   map[string]pendingAuthorization
}

// resolve authenticates the subject and rejects unknown servers before processing OAuth inputs.
func (h *oauthHandler) resolve(w http.ResponseWriter, r *http.Request) (string, oauthServerConfig, bool) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	namespace, err := h.namespace(r)
	if err != nil || strings.TrimSpace(namespace) == "" {
		http.Error(w, "Unable to resolve authenticated namespace", http.StatusForbidden)
		return "", oauthServerConfig{}, false
	}
	resolved, err := h.client.listServerConfigs(r.Context(), namespace, nil)
	if err != nil {
		http.Error(w, "Unable to read MCP configuration", http.StatusInternalServerError)
		return "", oauthServerConfig{}, false
	}
	config, ok := resolved.configs[r.PathValue("server")]
	if !ok || config.Authorization == nil {
		http.NotFound(w, r)
		return "", oauthServerConfig{}, false
	}
	server, err := resolveOAuthConfig(Connector{Name: config.Name, Namespace: config.Namespace, Endpoint: config.Endpoint, Authorization: config.Authorization})
	if err != nil {
		http.Error(w, "Invalid MCP OAuth configuration", http.StatusInternalServerError)
		return "", oauthServerConfig{}, false
	}
	return namespace, server, true
}

// connect creates a browser-bound state and S256 PKCE challenge before redirecting to consent.
func (h *oauthHandler) connect(w http.ResponseWriter, r *http.Request) {
	namespace, server, ok := h.resolve(w, r)
	if !ok {
		return
	}
	name := r.PathValue("server")
	state, verifier := oauth2.GenerateVerifier(), oauth2.GenerateVerifier()
	now := time.Now()

	// Prune expired attempts and remember only the authorization inputs needed at callback time.
	h.mu.Lock()
	for key, pending := range h.pending {
		if !now.Before(pending.expires) {
			delete(h.pending, key)
		}
	}
	h.pending[state] = pendingAuthorization{namespace: namespace, server: name, key: server.credentialKey(name), verifier: verifier, expires: now.Add(oauthStateLifetime), config: server}
	h.mu.Unlock()

	// A per-server HttpOnly cookie prevents another browser from completing this login attempt.
	cookie := authorizationCookie(name, server.OAuth.RedirectURL, state)
	http.SetCookie(w, cookie)
	options := slices.Clone(server.AuthCodeOptions)
	options = append(options, oauth2.AccessTypeOffline, oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("state", state))
	http.Redirect(w, r, server.OAuth.AuthCodeURL(state, options...), http.StatusFound)
}

// callback validates browser, subject and server binding before exchanging a one-use authorization code.
func (h *oauthHandler) callback(w http.ResponseWriter, r *http.Request) {
	namespace, server, ok := h.resolve(w, r)
	if !ok {
		return
	}
	name, state := r.PathValue("server"), r.URL.Query().Get("state")
	cookie, err := r.Cookie(authorizationCookie(name, server.OAuth.RedirectURL, "").Name)
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(state)) != 1 {
		http.Error(w, "Invalid OAuth state", http.StatusBadRequest)
		return
	}

	// Consume matching state exactly once, without trusting a namespace or server from the callback query.
	h.mu.Lock()
	pending, exists := h.pending[state]
	valid := exists && pending.namespace == namespace && pending.server == name && time.Now().Before(pending.expires)
	if valid {
		delete(h.pending, state)
	}
	h.mu.Unlock()
	if !valid {
		http.Error(w, "OAuth authorization expired or does not match this user", http.StatusBadRequest)
		return
	}
	cookie = authorizationCookie(name, server.OAuth.RedirectURL, "")
	cookie.MaxAge = -1
	http.SetCookie(w, cookie)
	if r.URL.Query().Get("error") != "" || r.URL.Query().Get("code") == "" {
		http.Error(w, "OAuth authorization was not granted; start again to connect", http.StatusBadRequest)
		return
	}

	// Serialize credential writes against refreshes so reconnecting cannot lose the new authorization.
	lock := h.provider.credentialLock(namespace, pending.key)
	lock.Lock()
	defer lock.Unlock()
	ctx, cancel := h.provider.tokenContext(r.Context(), pending.config)
	defer cancel()
	token, err := pending.config.OAuth.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(pending.verifier))
	if err != nil {
		http.Error(w, "OAuth token exchange failed; start again to connect", http.StatusBadGateway)
		return
	}

	// A fresh authorization replaces the account; do not graft on an old account's refresh token.
	if err := h.provider.store.Save(ctx, namespace, pending.key, token); err != nil {
		http.Error(w, "Unable to persist MCP authorization", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("MCP account connected. Return to the chat and send your request again."))
}

// authorizationCookie confines OAuth state to the configured callback path and browser session.
func authorizationCookie(server, redirectURL, state string) *http.Cookie {
	digest := sha256.Sum256([]byte(server))
	callback, _ := url.Parse(redirectURL)
	return &http.Cookie{
		Name: "hastekit_mcp_oauth_" + hex.EncodeToString(digest[:8]), Value: state,
		Path: callback.Path, HttpOnly: true, Secure: callback.Scheme == "https",
		SameSite: http.SameSiteLaxMode, MaxAge: int(oauthStateLifetime.Seconds()),
	}
}
