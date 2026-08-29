package mcpclient

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"golang.org/x/oauth2"
)

// serverConn is everything needed to open a session with one MCP server: which
// server, reached how, with what credentials. It travels as one value rather
// than as a handful of positional arguments, because every layer below — the
// transport, the pool, the pool's key — needs the whole set, and a transport
// that reads none of a field still has to pass it along.
//
// Which fields matter depends on Transport: the http transports read Endpoint
// and Headers, stdio reads Command and Env.
type serverConn struct {
	// Name is the connector this describes — the one NewClient required, and
	// the one a failure is reported under. It is what makes a key or a log
	// line legible; it is not what makes either of them correct, since nothing
	// stops two clients being built with one name.
	Name string

	Transport string

	// Endpoint and Headers address an http server.
	Endpoint string
	Headers  map[string]string

	// Command is a stdio server's argv, program first, and Env is added to the
	// environment it inherits.
	Command []string
	Env     map[string]string

	// TokenSource is where the bearer for an http server comes from, resolved
	// once per call by the client's CredentialProvider. It is asked for a
	// token on every request the connection makes, so a token that expires
	// while a run is paused is refreshed rather than replayed.
	//
	// Deliberately absent from key: a token rotates and a principal does not,
	// so keying on it would strand a pooled connection on every refresh.
	// Principal is what identifies the credentials instead.
	TokenSource oauth2.TokenSource

	// Principal is who TokenSource's tokens belong to — see
	// Credential.Principal. Empty when there are no credentials, and empty
	// when a provider declined to say, which is the case poolable rules out.
	Principal string

	DisableStandaloneSSE bool
}

// key identifies the server this connects to, and is what the connection pool
// and the schema cache are keyed by. Everything that selects a different server
// — or the same one under different credentials — has to be in it, or two of
// them would quietly share one connection and one set of cached schemas.
func (c serverConn) key() string {
	return fmt.Sprintf("%s|%s|%s", c.Name, c.Principal, c.digest())
}

// digest fingerprints everything that decides which server this reaches and
// what it presents on arrival: the transport and address, and the credentials.
//
// It is one value rather than five fields because a key only ever needs these
// to differ, never to be read back — the name and principal in front of it are
// what make the key legible. Folding them together keeps that property and
// stops the key growing a field at a time.
//
// The name alone would not do. globalPool is package-level and shared by every
// client in the process, and nothing stops two of them being named the same
// while addressing different servers — one tenant's jira and another's. That
// collision would hand a live, authenticated session to the wrong server.
func (c serverConn) digest() string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		c.Transport,
		c.Endpoint,
		strings.Join(c.Command, " "),
		sortedPairs(c.Headers),
		sortedPairs(c.Env),
	}, "|")))
	return hex.EncodeToString(sum[:12])
}

// poolable reports whether this connection may be shared with the next call
// that describes the same server.
//
// A session an http server authenticated is bound to the identity that opened
// it, so it can only be reused by that identity. While the token travelled in
// a header, key told two principals apart on its own. A token source does not
// appear in key, so a connection carrying one is poolable only when its
// provider named the principal — otherwise the pool would hand one user the
// session another authenticated.
func (c serverConn) poolable() bool {
	return c.TokenSource == nil || c.Principal != ""
}

// credentialDigest fingerprints what authenticates this connection, so a key
// can tell two requesters apart without carrying what either of them presents.
//
// Keys are not private. This one is logged when the pool closes an idle
// connection, and it is also the prefix of every SchemaCache key, which reaches
// whatever store was injected — a shared Redis, its slow log, whatever scrapes
// its metrics. A header or an env var is a credential as often as not
// (WithHeaders templating a bearer out of the run context is the documented way
// to pass one), so it is fingerprinted rather than reproduced. Two different
// credentials still produce two different digests, which is all a key needs.
func credentialDigest(headers, env map[string]string) string {
	if len(headers) == 0 && len(env) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(sortedPairs(headers) + "|" + sortedPairs(env)))
	return hex.EncodeToString(sum[:8])
}

// isStdio reports whether this server is a child process rather than a URL.
func (c serverConn) isStdio() bool { return c.Transport == TransportStdio }

// validate catches a half-configured server at connect time, where the message
// can say what is missing, rather than at the transport where it surfaces as a
// failure to dial "" or to run "".
func (c serverConn) validate() error {
	if c.isStdio() {
		if len(c.Command) == 0 || c.Command[0] == "" {
			return fmt.Errorf("mcp: the %s transport needs a command to run (see WithCommand)", TransportStdio)
		}
		return nil
	}
	if c.Endpoint == "" {
		return fmt.Errorf("mcp: the %s transport needs an endpoint to connect to", c.Transport)
	}
	return nil
}

// childEnv is the environment a stdio server's process is started with: the
// one this process has, plus Env. Inherited rather than replaced because the
// command has to be findable — a server launched as "npx" needs PATH — and a
// caller who wants a variable gone can still set it to empty.
func (c serverConn) childEnv() []string {
	if len(c.Env) == 0 {
		return nil // nil means "inherit", which is what exec does by default
	}
	env := os.Environ()
	for _, k := range slices.Sorted(maps.Keys(c.Env)) {
		env = append(env, k+"="+c.Env[k])
	}
	return env
}

// requesterKey identifies who is asking, for the purpose of a cache entry that
// another run might read.
//
// The principal when a credential provider named one. Otherwise the credentials
// themselves, which is what told one requester from another before there was a
// principal — a static per-tenant header, a templated token, a stdio server's
// env.
func (c serverConn) requesterKey() string {
	if c.Principal != "" {
		return c.Principal
	}
	return credentialDigest(c.Headers, c.Env)
}

// sortedPairs renders a map deterministically, for use in a key.
func sortedPairs(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(m)) {
		b.WriteString(k + "=" + m[k] + ";")
	}
	return b.String()
}
