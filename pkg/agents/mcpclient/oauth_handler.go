package mcpclient

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

const oauthStateLifetime = 10 * time.Minute

// completeAuthorization finishes an authorization attempt with what the
// authorization server sent the browser back with, saving the grant.
type completeAuthorization func(ctx context.Context, result *auth.AuthorizationResult) error

// pendingAuthorization binds an expiring authorization attempt to its subject and server.
// complete saves under the client that issued the grant; cancel ends an attempt
// that is abandoned, for flows that hold one open.
type pendingAuthorization struct {
	namespace, server string
	expires           time.Time
	complete          completeAuthorization
	cancel            func()
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

// connect starts an authorization attempt and redirects the browser to consent.
func (h *oauthHandler) connect(w http.ResponseWriter, r *http.Request) {
	namespace, server, ok := h.resolve(w, r)
	if !ok {
		return
	}
	name := r.PathValue("server")

	var (
		authURL  string
		complete completeAuthorization
		cancel   func()
		err      error
	)
	if server.Discovered {
		authURL, complete, cancel, err = h.startDiscovered(r.Context(), namespace, server)
	} else {
		authURL, complete = h.startConfigured(namespace, server)
	}
	if err != nil {
		// Discovery and registration errors can carry the servers' response bodies: log, don't echo.
		slog.WarnContext(r.Context(), "MCP OAuth could not start", slog.String("server", name), slog.Any("error", err))
		http.Error(w, "Unable to start sign-in: the MCP server's authorization could not be discovered, or no client could be registered with it", http.StatusBadGateway)
		return
	}
	parsed, err := url.Parse(authURL)
	state := ""
	if err == nil {
		state = parsed.Query().Get("state")
	}
	if state == "" {
		if cancel != nil {
			cancel()
		}
		http.Error(w, "Unable to start sign-in", http.StatusInternalServerError)
		return
	}
	now := time.Now()

	// Prune expired attempts and remember only what the callback needs.
	h.mu.Lock()
	for key, pending := range h.pending {
		if !now.Before(pending.expires) {
			if pending.cancel != nil {
				pending.cancel()
			}
			delete(h.pending, key)
		}
	}
	h.pending[state] = pendingAuthorization{namespace: namespace, server: name, expires: now.Add(oauthStateLifetime), complete: complete, cancel: cancel}
	h.mu.Unlock()

	// A per-server HttpOnly cookie prevents another browser from completing this login attempt.
	http.SetCookie(w, authorizationCookie(name, server.OAuth.RedirectURL, state))
	http.Redirect(w, r, authURL, http.StatusFound)
}

// startConfigured begins an authorization code flow with PKCE against the
// endpoints the config names.
func (h *oauthHandler) startConfigured(namespace string, server oauthServerConfig) (string, completeAuthorization) {
	state, verifier := oauth2.GenerateVerifier(), oauth2.GenerateVerifier()
	options := slices.Clone(server.AuthCodeOptions)
	options = append(options, oauth2.AccessTypeOffline, oauth2.S256ChallengeOption(verifier))
	complete := func(ctx context.Context, result *auth.AuthorizationResult) error {
		// Serialize credential writes against refreshes so reconnecting cannot lose the new authorization.
		lock := h.provider.credentialLock(namespace, server.Key)
		lock.Lock()
		defer lock.Unlock()
		ctx, cancel := h.provider.tokenContext(ctx, server)
		defer cancel()
		token, err := server.OAuth.Exchange(ctx, result.Code, oauth2.VerifierOption(verifier))
		if err != nil {
			return err
		}
		// A fresh authorization replaces the account; do not graft on an old account's refresh token.
		return h.provider.store.Save(ctx, namespace, server.Key, token)
	}
	return server.OAuth.AuthCodeURL(state, options...), complete
}

// startDiscovered runs the MCP SDK's authorization flow for a server whose
// endpoints the config leaves out: it discovers the authorization server from
// the MCP server's metadata, registers a client there unless the config names
// one or an earlier authorization registered one, and performs the
// authorization code grant with PKCE and the resource indicator.
//
// The SDK's flow is one call that asks for the authorization code midway. Here
// the code arrives on a later request, the callback, so the flow runs in the
// background from connect: its request for the code is the authorization URL
// connect redirects to, and complete hands it the code the callback received.
func (h *oauthHandler) startDiscovered(ctx context.Context, namespace string, server oauthServerConfig) (string, completeAuthorization, func(), error) {
	clients, err := h.provider.oauthClients()
	if err != nil {
		return "", nil, nil, err
	}
	httpClient := h.provider.httpClient(server)
	config := &auth.AuthorizationCodeHandlerConfig{
		RedirectURL:         server.OAuth.RedirectURL,
		Client:              httpClient,
		RequestRefreshToken: true,
	}
	if scopes := slices.Clone(server.OAuth.Scopes); len(scopes) > 0 {
		config.ScopeFilter = func([]string) []string { return scopes }
	}

	// The client: the config's own, the one an earlier authorization registered, or a new registration.
	reused := false
	switch stored, err := clients.LoadOAuthClient(ctx, namespace, server.Key); {
	case server.OAuth.ClientID != "":
		config.PreregisteredClient = clientCredentials(server.OAuth.ClientID, server.OAuth.ClientSecret)
	case err == nil:
		config.PreregisteredClient = clientCredentials(stored.ClientID, stored.ClientSecret)
		reused = true
	case errors.Is(err, ErrCredentialNotFound):
		config.DynamicClientRegistrationConfig = &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
			RedirectURIs:            []string{server.OAuth.RedirectURL},
			ClientName:              "HasteKit",
			GrantTypes:              []string{"authorization_code", "refresh_token"},
			ResponseTypes:           []string{"code"},
			TokenEndpointAuthMethod: "none",
		}}
	default:
		return "", nil, nil, err
	}

	// The flow's request for the code becomes connect's redirect; the callback answers it.
	urls := make(chan string, 1)
	codes := make(chan *auth.AuthorizationResult, 1)
	config.AuthorizationCodeFetcher = func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		urls <- withAuthCodeParams(args.URL, server.AuthCodeParams)
		select {
		case result := <-codes:
			return result, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	// The grant and the client it was issued to, kept once the exchange succeeds.
	var granted *oauth2.Config
	var token *oauth2.Token
	config.NewTokenSource = func(_ context.Context, cfg *oauth2.Config, t *oauth2.Token) (oauth2.TokenSource, error) {
		granted, token = cfg, t
		return oauth2.StaticTokenSource(t), nil
	}
	handler, err := auth.NewAuthorizationCodeHandler(config)
	if err != nil {
		return "", nil, nil, err
	}

	// The MCP server's own answer to an unauthenticated request says where its
	// metadata is (WWW-Authenticate); without one, the SDK tries the well-known URLs.
	probe, err := http.NewRequestWithContext(ctx, http.MethodGet, server.Endpoint, nil)
	if err != nil {
		return "", nil, nil, err
	}
	probe.Header.Set("Accept", "application/json, text/event-stream")
	response, err := httpClient.Do(probe)
	if err != nil {
		return "", nil, nil, err
	}

	flowCtx, cancel := context.WithTimeout(context.Background(), oauthStateLifetime)
	done := make(chan error, 1)
	go func() {
		defer cancel()
		err := handler.Authorize(flowCtx, probe, response)
		var retrieve *oauth2.RetrieveError
		switch {
		case err == nil:
			err = h.saveDiscovered(namespace, server, granted, token)
		case reused && errors.As(err, &retrieve) && retrieve.ErrorCode == "invalid_client":
			// The authorization server no longer knows the registered client: the next attempt registers again.
			_ = clients.DeleteOAuthClient(context.Background(), namespace, server.Key)
		}
		done <- err
	}()

	// Discovery and registration happen before the flow asks for the code; bound them.
	timer := time.NewTimer(h.provider.timeout)
	defer timer.Stop()
	select {
	case authURL := <-urls:
		complete := func(ctx context.Context, result *auth.AuthorizationResult) error {
			codes <- result
			select {
			case err := <-done:
				return err
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return authURL, complete, cancel, nil
	case err := <-done:
		return "", nil, nil, err
	case <-timer.C:
		cancel()
		return "", nil, nil, errors.New("MCP OAuth discovery timed out")
	case <-ctx.Done():
		cancel()
		return "", nil, nil, ctx.Err()
	}
}

// saveDiscovered keeps the grant and the client it was issued to, which
// refreshing it needs. The client is saved first: a grant is what marks the
// account connected, and using it needs the client.
func (h *oauthHandler) saveDiscovered(namespace string, server oauthServerConfig, granted *oauth2.Config, token *oauth2.Token) error {
	if granted == nil || token == nil {
		return errors.New("MCP OAuth authorization produced no token")
	}
	clients, err := h.provider.oauthClients()
	if err != nil {
		return err
	}
	lock := h.provider.credentialLock(namespace, server.Key)
	lock.Lock()
	defer lock.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), h.provider.timeout)
	defer cancel()
	client := &OAuthClient{
		ClientID: granted.ClientID, ClientSecret: granted.ClientSecret,
		AuthURL: granted.Endpoint.AuthURL, TokenURL: granted.Endpoint.TokenURL, AuthStyle: granted.Endpoint.AuthStyle,
		Scopes: granted.Scopes,
	}
	if err := clients.SaveOAuthClient(ctx, namespace, server.Key, client); err != nil {
		return err
	}
	return h.provider.store.Save(ctx, namespace, server.Key, token)
}

// clientCredentials describes a client the SDK's flow uses as preregistered.
func clientCredentials(id, secret string) *oauthex.ClientCredentials {
	credentials := &oauthex.ClientCredentials{ClientID: id}
	if secret != "" {
		credentials.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: secret}
	}
	return credentials
}

// withAuthCodeParams adds a config's extra consent parameters to an
// authorization URL the SDK built, never replacing one it set.
func withAuthCodeParams(raw string, params map[string]string) string {
	if len(params) == 0 {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	query := u.Query()
	for key, value := range params {
		if !query.Has(key) {
			query.Set(key, value)
		}
	}
	u.RawQuery = query.Encode()
	return u.String()
}

// callback validates browser, subject and server binding before completing a one-use authorization attempt.
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
		if pending.cancel != nil {
			pending.cancel()
		}
		http.Error(w, "OAuth authorization was not granted; start again to connect", http.StatusBadRequest)
		return
	}

	result := &auth.AuthorizationResult{Code: r.URL.Query().Get("code"), State: state, Iss: r.URL.Query().Get("iss")}
	if err := pending.complete(r.Context(), result); err != nil {
		// Never propagate OAuth response bodies: providers may echo sensitive values in them.
		slog.WarnContext(r.Context(), "MCP OAuth could not complete", slog.String("server", name), slog.Any("error", err))
		http.Error(w, "OAuth token exchange failed; start again to connect", http.StatusBadGateway)
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
