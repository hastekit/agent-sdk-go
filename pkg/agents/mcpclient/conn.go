package mcpclient

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
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
	Transport string

	// Endpoint and Headers address an http server.
	Endpoint string
	Headers  map[string]string

	// Command is a stdio server's argv, program first, and Env is added to the
	// environment it inherits.
	Command []string
	Env     map[string]string

	DisableStandaloneSSE bool
}

// key identifies the server this connects to, and is what the connection pool
// and the schema cache are keyed by. Everything that selects a different server
// — or the same one under different credentials — has to be in it, or two of
// them would quietly share one connection and one set of cached schemas.
func (c serverConn) key() string {
	return fmt.Sprintf("%s|%s|%s|%s|%s",
		c.Transport, c.Endpoint, sortedPairs(c.Headers), strings.Join(c.Command, " "), sortedPairs(c.Env))
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
