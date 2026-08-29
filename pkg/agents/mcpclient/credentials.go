package mcpclient

import (
	"context"
	"fmt"

	"golang.org/x/oauth2"
)

// A per-run credential reaches an MCP server one of two ways. WithHeaders
// templates a value out of the run context, which is enough when the run
// already carries the token. A CredentialProvider is for when it does not —
// when the token has to be fetched from a store, minted, or refreshed, and
// when it expires partway through a run that is waiting on a person.
//
// The difference that matters is *when*. A templated header is fixed the
// moment the connection is described; a token source is asked again on every
// request the connection makes, so a run that pauses for an hour and resumes
// presents a token minted after the pause rather than before it.

// Connector identifies the MCP server a credential is being asked for. One
// provider usually serves an agent's whole set of them — a token store does
// not want to be instantiated once per server — so it has to be told which one
// each call is about.
//
// Name is that identity: the name the connector was constructed with, the same
// one it is reported under in the agent's prompt. Endpoint comes along because
// a store keyed by resource URI would otherwise have to keep its own map from
// one to the other, duplicating configuration that already lives here.
type Connector struct {
	Name     string
	Endpoint string
}

// Credential is what a provider resolved for one connector on one run: where
// the tokens come from, and whose they are.
//
// The two travel together because they are answered together and are useless
// apart. The key cannot be read off a token — it is needed when the connection
// is described, and a token does not exist until a request is on the wire —
// so it is settled here, at the one moment the provider knows both.
type Credential struct {
	// TokenSource is asked for a token on every HTTP request the resulting
	// connection makes, which is what lets an oauth2.ReuseTokenSource refresh
	// underneath a long-running or paused run without anything above having to
	// notice.
	//
	// Nil means "no auth", and is not an error: a provider that only handles
	// some of an agent's servers says nothing about the rest.
	TokenSource oauth2.TokenSource

	// Principal is who these credentials are for, and is what lets the
	// connection be pooled. A live MCP session is bound, server-side, to the
	// identity that opened it, and the pool hands sessions back by key. While
	// the token travelled in a header it was part of that key by accident, so
	// two users could not collide; a token source is not part of any key, so
	// without this the pool would hand user B the session user A authenticated.
	//
	// Leaving it empty is therefore not a shortcut but a choice: the
	// connection is not pooled at all, and every call opens its own.
	//
	// Whoever the credentials were looked up *for* — usually a user id, and
	// something like "tenant:user" where one id is not enough on its own. Not
	// the token's subject: an access token is opaque to a client and often not
	// a JWT at all, and under client credentials its sub is the client rather
	// than anyone on whose behalf the call is made. A provider already holds
	// this value, having just used it to find the token.
	//
	// It has to stay the same across a refresh, or every rotation would strand
	// a pooled connection and open a new one. The connector is already part of
	// the pool key, so it does not need to be combined in here. It is used as a
	// map key and never sent anywhere, but it does end up in pool and cache
	// keys, so it should not be a secret.
	Principal string
}

// CredentialProvider resolves the access token for one MCP server.
//
// Resolve is called once per tool call — and once per listing — with the
// connector being reached and that run's context, so one provider serves every
// server an agent has and can return a different credential for each, per user.
//
// Returning nil, or a Credential with no TokenSource, means "no auth".
//
// Only the http transports present a token. A stdio server is a child process
// with no request to put a header on; give it credentials through WithEnv.
type CredentialProvider interface {
	Resolve(ctx context.Context, connector Connector, runContext map[string]any) (*Credential, error)
}

// CredentialProviderFn adapts a function to CredentialProvider.
type CredentialProviderFn func(ctx context.Context, connector Connector, runContext map[string]any) (*Credential, error)

func (f CredentialProviderFn) Resolve(ctx context.Context, connector Connector, runContext map[string]any) (*Credential, error) {
	return f(ctx, connector, runContext)
}

// WithCredentialProvider sets where this server's access token comes from.
//
//	mcpclient.NewClient(ctx, "calendar", endpoint,
//		mcpclient.WithTransport(mcpclient.TransportStreamableHTTP),
//		mcpclient.WithCredentialProvider(myTokenStore),
//	)
//
// A provider that returns a Credential with no Principal gets a fresh
// connection per call rather than a pooled one — see Credential.Principal.
func WithCredentialProvider(provider CredentialProvider) McpServerOption {
	return func(srv *MCPClient) {
		srv.credentials = provider
	}
}

// credentialsFor resolves this run's credentials for this server, unpacked into
// the two things a connection needs: where its tokens come from, and which
// principal they belong to. Both are zero when there is no provider, or when
// the provider had nothing to say about this run.
//
// It runs wherever connFor does, which under a durable runtime is inside the
// activity or step — never in the workflow. That is deliberate. A workflow
// journals what it sees, so a token resolved there would be written into
// history and replayed from it; resolving here means the workflow only ever
// carries the run context that names the user, and the token is fetched fresh
// on each attempt.
func (srv *MCPClient) credentialsFor(ctx context.Context, runContext map[string]any) (oauth2.TokenSource, string, error) {
	if srv.credentials == nil {
		return nil, "", nil
	}

	connector := Connector{Name: srv.Name, Endpoint: srv.Endpoint}

	cred, err := srv.credentials.Resolve(ctx, connector, runContext)
	if err != nil {
		return nil, "", fmt.Errorf("mcp: resolving credentials for %q: %w", srv.Name, err)
	}
	if cred == nil || cred.TokenSource == nil {
		// "No auth" — the provider had nothing to say about this run. A
		// principal with no credentials to identify is meaningless, so it is
		// dropped along with them.
		return nil, "", nil
	}

	return cred.TokenSource, cred.Principal, nil
}
