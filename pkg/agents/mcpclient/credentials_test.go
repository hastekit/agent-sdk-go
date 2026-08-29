package mcpclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

// stubProvider is one provider serving every connector, which is the shape a
// real token store has: it hands out a token per (connector, user) and records
// what it was asked about.
type stubProvider struct {
	mu       sync.Mutex
	tokens   map[string]string // "connector/user" -> token
	asked    []string          // "connector/user", in order
	keyed    bool              // whether it names the principal it resolved for
	failWith error
}

func (p *stubProvider) Resolve(_ context.Context, connector Connector, runContext map[string]any) (*Credential, error) {
	if p.failWith != nil {
		return nil, p.failWith
	}
	user, _ := runContext["user"].(string)
	lookup := connector.Name + "/" + user

	p.mu.Lock()
	p.asked = append(p.asked, lookup)
	p.mu.Unlock()

	token, ok := p.tokens[lookup]
	if !ok {
		return nil, nil // "no auth" for a pair this provider says nothing about
	}
	return &Credential{
		TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token, TokenType: "Bearer"}),
		Principal:   p.principal(user),
	}, nil
}

// principal is empty unless this provider names who it resolved for — an
// unidentified credential is the one the pool must not share.
func (p *stubProvider) principal(user string) string {
	if p.keyed {
		return user
	}
	return ""
}

func (p *stubProvider) askedAbout() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.asked...)
}

// keyed is the same provider, saying whose credentials it resolved.
func keyed(p *stubProvider) *stubProvider {
	p.keyed = true
	return p
}

func newTestClient(t *testing.T, endpoint string, opts ...McpServerOption) *MCPClient {
	t.Helper()
	client, err := NewClient(context.Background(), "creds", endpoint, opts...)
	require.NoError(t, err)
	return client
}

// The token is asked for per request, not baked into the connection, so a run
// that pauses and resumes presents whatever the source returns at that moment.
func TestTokenSourceIsPresentedAsABearer(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL,
		WithTransport(TransportStreamableHTTP),
		WithCredentialProvider(&stubProvider{tokens: map[string]string{"creds/ada": "tok-ada"}}),
	)

	conn, err := client.connFor(context.Background(), map[string]any{"user": "ada"})
	require.NoError(t, err)

	_, err = connect(context.Background(), conn)
	require.Error(t, err)
	assert.Equal(t, "Bearer tok-ada", got)
}

// A run's own credential is the more specific of the two.
func TestTokenSourceOverridesAConfiguredAuthorizationHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL,
		WithTransport(TransportStreamableHTTP),
		WithHeaders(map[string]string{"Authorization": "Bearer static", "X-Trace": "keep-me"}),
		WithCredentialProvider(&stubProvider{tokens: map[string]string{"creds/ada": "tok-ada"}}),
	)

	conn, err := client.connFor(context.Background(), map[string]any{"user": "ada"})
	require.NoError(t, err)
	assert.Equal(t, "keep-me", conn.Headers["X-Trace"], "other headers are untouched")

	_, err = connect(context.Background(), conn)
	require.Error(t, err)
	assert.Equal(t, "Bearer tok-ada", got)
}

// A provider that says nothing about this run is not a failure.
func TestNilTokenSourceMeansNoAuth(t *testing.T) {
	client := newTestClient(t, "https://example.test/mcp",
		WithTransport(TransportStreamableHTTP),
		WithCredentialProvider(&stubProvider{tokens: map[string]string{"creds/ada": "tok-ada"}}),
	)

	conn, err := client.connFor(context.Background(), map[string]any{"user": "grace"})
	require.NoError(t, err)
	assert.Nil(t, conn.TokenSource)
	assert.True(t, conn.poolable(), "a connection with no credentials is shared as it always was")
}

func TestAProviderThatFailsFailsTheCall(t *testing.T) {
	client := newTestClient(t, "https://example.test/mcp",
		WithTransport(TransportStreamableHTTP),
		WithCredentialProvider(&stubProvider{failWith: errors.New("token store unreachable")}),
	)

	_, err := client.connFor(context.Background(), map[string]any{"user": "ada"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token store unreachable")
	assert.Contains(t, err.Error(), `"creds"`, "the message says which server could not be reached for")
}

// A token that cannot be produced is an authorization failure, and reaches the
// agent as one — so the prompt tells the user to reconnect rather than to wait.
func TestAFailingTokenSourceIsClassifiedAsAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	failing := oauth2.ReuseTokenSource(nil, tokenSourceFn(func() (*oauth2.Token, error) {
		return nil, errors.New("refresh failed: invalid_grant")
	}))

	_, err := connect(context.Background(), serverConn{
		Transport:   TransportStreamableHTTP,
		Endpoint:    srv.URL,
		TokenSource: failing,
		Principal:   "ada",
	})
	require.Error(t, err)

	var te *agents.ToolsetError
	require.True(t, errors.As(err, &te))
	assert.Equal(t, agents.ToolsetErrorAuth, te.Kind)
}

type tokenSourceFn func() (*oauth2.Token, error)

func (f tokenSourceFn) Token() (*oauth2.Token, error) { return f() }

// Two users of one server must never share a session: an MCP session is bound
// server-side to the identity that opened it.
func TestPoolKeySeparatesPrincipals(t *testing.T) {
	provider := keyed(&stubProvider{tokens: map[string]string{"creds/ada": "tok-ada", "creds/grace": "tok-grace"}})
	client := newTestClient(t, "https://example.test/mcp",
		WithTransport(TransportStreamableHTTP),
		WithCredentialProvider(provider),
	)

	ada, err := client.connFor(context.Background(), map[string]any{"user": "ada"})
	require.NoError(t, err)
	grace, err := client.connFor(context.Background(), map[string]any{"user": "grace"})
	require.NoError(t, err)

	assert.Equal(t, "ada", ada.Principal)
	assert.NotEqual(t, ada.key(), grace.key(), "one user must not be handed another's session")
	assert.True(t, ada.poolable())

	// The key identifies the principal, not the token, so a refresh does not
	// strand the pooled connection.
	again, err := client.connFor(context.Background(), map[string]any{"user": "ada"})
	require.NoError(t, err)
	assert.Equal(t, ada.key(), again.key())
}

// Without a Principal there is nothing to tell one user's connection from
// another's, so it is not pooled at all.
func TestUnidentifiedCredentialsAreNotPooled(t *testing.T) {
	client := newTestClient(t, "https://example.test/mcp",
		WithTransport(TransportStreamableHTTP),
		WithCredentialProvider(&stubProvider{tokens: map[string]string{"creds/ada": "tok-ada"}}),
	)

	conn, err := client.connFor(context.Background(), map[string]any{"user": "ada"})
	require.NoError(t, err)

	require.NotNil(t, conn.TokenSource)
	assert.Empty(t, conn.Principal)
	assert.False(t, conn.poolable())
}

// The provider is consulted per call with that call's run context — which is
// what lets one client serve every user, and what keeps the lookup on the
// activity side of a durable boundary.
func TestProviderIsAskedPerCallWithTheRunContext(t *testing.T) {
	provider := &stubProvider{tokens: map[string]string{"creds/ada": "tok-ada", "creds/grace": "tok-grace"}}
	client := newTestClient(t, "https://example.test/mcp",
		WithTransport(TransportStreamableHTTP),
		WithCredentialProvider(provider),
	)

	for _, user := range []string{"ada", "grace", "ada"} {
		_, err := client.connFor(context.Background(), map[string]any{"user": user})
		require.NoError(t, err)
	}

	assert.Equal(t, []string{"creds/ada", "creds/grace", "creds/ada"}, provider.askedAbout())
}

// Nothing a durable runtime serializes may carry a token. The tools that cross
// into the workflow are BaseTools, and the connection — token source and all —
// stays behind in the activity.
func TestListedToolsCarryNoCredentialAcrossTheBoundary(t *testing.T) {
	provider := keyed(&stubProvider{tokens: map[string]string{"creds/ada": "tok-ada"}})
	client := newTestClient(t, "https://example.test/mcp",
		WithTransport(TransportStreamableHTTP),
		WithCredentialProvider(provider),
	)

	conn, err := client.connFor(context.Background(), map[string]any{"user": "ada"})
	require.NoError(t, err)
	require.NotNil(t, conn.TokenSource, "the connection itself does hold one")

	for _, tool := range client.buildLazyTools([]*mcp.Tool{{Name: "search"}}, nil, conn) {
		base, err := tool.GetBaseTool()
		require.NoError(t, err)

		encoded, err := sonic.MarshalString(base)
		require.NoError(t, err)
		assert.NotContains(t, encoded, "tok-ada")
		assert.NotContains(t, encoded, "ada", "not even the principal's name rides along")
	}
}

// The point of passing the connector: one provider serves every server the
// agent has, and returns a different token for each. Without it a token store
// would have to be instantiated once per connector, and would still be handing
// out whichever credential it happened to be built with.
func TestOneProviderServesEveryConnector(t *testing.T) {
	provider := keyed(&stubProvider{tokens: map[string]string{
		"calendar/ada": "tok-calendar",
		"jira/ada":     "tok-jira",
	}})

	run := map[string]any{"user": "ada"}

	calendar, err := NewClient(context.Background(), "calendar", "https://calendar.test/mcp",
		WithTransport(TransportStreamableHTTP), WithCredentialProvider(provider))
	require.NoError(t, err)

	jira, err := NewClient(context.Background(), "jira", "https://jira.test/mcp",
		WithTransport(TransportStreamableHTTP), WithCredentialProvider(provider))
	require.NoError(t, err)

	calConn, err := calendar.connFor(context.Background(), run)
	require.NoError(t, err)
	jiraConn, err := jira.connFor(context.Background(), run)
	require.NoError(t, err)

	calToken, err := calConn.TokenSource.Token()
	require.NoError(t, err)
	jiraToken, err := jiraConn.TokenSource.Token()
	require.NoError(t, err)

	assert.Equal(t, "tok-calendar", calToken.AccessToken)
	assert.Equal(t, "tok-jira", jiraToken.AccessToken)

	assert.Equal(t, []string{"calendar/ada", "jira/ada"}, provider.askedAbout())

	// The same user on two servers is still two sessions: the connector is
	// part of the pool key in its own right.
	assert.NotEqual(t, calConn.key(), jiraConn.key())
}

// The endpoint travels with the name so a store keyed by resource URI does not
// have to keep its own map from one to the other.
func TestProviderIsToldTheEndpointToo(t *testing.T) {
	var seen Connector

	client, err := NewClient(context.Background(), "calendar", "https://calendar.test/mcp",
		WithTransport(TransportStreamableHTTP),
		WithCredentialProvider(CredentialProviderFn(
			func(_ context.Context, connector Connector, _ map[string]any) (*Credential, error) {
				seen = connector
				return nil, nil
			})),
	)
	require.NoError(t, err)

	_, err = client.connFor(context.Background(), nil)
	require.NoError(t, err)

	assert.Equal(t, Connector{Name: "calendar", Endpoint: "https://calendar.test/mcp"}, seen)
}

// Keys are not private: the pool logs one when it closes an idle connection,
// and every SchemaCache key is prefixed with the connection's own. Neither may
// carry what authenticates the connection.
func TestKeysCarryNoCredential(t *testing.T) {
	const (
		bearer  = "eyJhbGciOiJI.SECRET.sig"
		ghToken = "ghp_supersecret"
		apiKey  = "sk-live-9f8e7d"
	)

	conn := serverConn{
		Name:      "calendar",
		Transport: TransportStreamableHTTP,
		Endpoint:  "https://cal.test/mcp?apiKey=" + apiKey,
		Headers:   map[string]string{"Authorization": "Bearer " + bearer},
		Env:       map[string]string{"GITHUB_TOKEN": ghToken},
	}

	for _, secret := range []string{bearer, ghToken, apiKey} {
		assert.NotContains(t, conn.key(), secret, "the pool key is also every cache key's prefix")
		assert.NotContains(t, conn.requesterKey(), secret)
	}
	assert.Contains(t, conn.key(), "calendar", "and it still says which connector it was")

	// Still identifying: a different credential is a different connection.
	other := conn
	other.Headers = map[string]string{"Authorization": "Bearer someone-else"}
	assert.NotEqual(t, conn.key(), other.key())
	assert.NotEqual(t, conn.requesterKey(), other.requesterKey())

	// And the same one is the same connection, across calls.
	same := conn
	same.Headers = map[string]string{"Authorization": "Bearer " + bearer}
	assert.Equal(t, conn.key(), same.key())
}

// The pool is package-level and shared by every client in the process, so the
// connector's name cannot be the whole of a pool key: two tenants' connectors
// are routinely named the same and addressed differently, and sharing a live
// session between them would send one tenant's call to the other's server.
func TestPoolKeySeparatesSameNamedConnectors(t *testing.T) {
	acme := serverConn{Name: "jira", Transport: TransportStreamableHTTP, Endpoint: "https://acme.atlassian.net/mcp"}
	globex := serverConn{Name: "jira", Transport: TransportStreamableHTTP, Endpoint: "https://globex.atlassian.net/mcp"}

	assert.NotEqual(t, acme.key(), globex.key())

	// Transport and command are part of it too.
	sse := acme
	sse.Transport = TransportSSE
	assert.NotEqual(t, acme.key(), sse.key())

	// And the same description is the same connection, every time.
	assert.Equal(t, acme.key(), serverConn{
		Name: "jira", Transport: TransportStreamableHTTP, Endpoint: "https://acme.atlassian.net/mcp",
	}.key())
}
