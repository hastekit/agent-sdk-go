package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type server struct {
	Name      string            `json:"-"`
	Endpoint  string            `json:"-"`
	Transport string            `json:"-"`
	Headers   map[string]string `json:"-"`

	Session               *mcp.ClientSession `json:"-"`
	Tools                 []*mcp.Tool        `json:"-"`
	Meta                  mcp.Meta           `json:"-"`
	ToolFilter            ToolFilter         `json:"-"`
	ApprovalRequiredTools []string           `json:"-"`
	DeferredTools         *ToolFilter        `json:"-"`
	ToolPrefix            string             `json:"-"`

	// Command is a stdio server's argv, program first, and Env is added to the
	// environment its process inherits. Both are ignored by the http transports.
	Command []string          `json:"-"`
	Env     map[string]string `json:"-"`

	CacheTTL             time.Duration `json:"-"`
	DisableStandaloneSSE bool          `json:"-"`
	schemaCache          SchemaCache   // injected cache (required for caching)
	defaultCacheScope    string

	// credentials resolves this server's access token per call
	credentials   CredentialProvider
	authorization *OAuthConfig
	namespace     string
}

func newServer(ctx context.Context, name string, endpoint string, options ...serverOption) (*server, error) {
	if name == "" {
		return nil, fmt.Errorf("name is required for mcp connector")
	}

	srv := &server{
		Name:     name,
		Endpoint: endpoint,
	}

	for _, option := range options {
		option(srv)
	}
	if srv.defaultCacheScope != "" && srv.defaultCacheScope != CacheScopePublic && srv.defaultCacheScope != CacheScopePrivate {
		return nil, fmt.Errorf("invalid default MCP cache scope %q: want public or private", srv.defaultCacheScope)
	}

	// Copied, not written into: withMeta stores the caller's own map, and two
	// clients built from one map would otherwise overwrite each other's
	// server_name — and the caller's map besides.
	meta := map[string]any{}
	maps.Copy(meta, srv.Meta)
	meta["server_name"] = srv.Name
	srv.Meta = meta

	return srv, nil
}

type serverOption func(*server)

func withHeaders(headers map[string]string) serverOption {
	return func(server *server) {
		server.Headers = headers
	}
}

// ToolFilter selects tools by their original server names, before any prefix
// is applied. An empty Include allows all tools. Exclude takes precedence over
// Include. Names are matched exactly.
type ToolFilter struct {
	Include []string `json:"include,omitempty"`
	Exclude []string `json:"exclude,omitempty"`
}

func (f ToolFilter) allows(name string) bool {
	return (len(f.Include) == 0 || slices.Contains(f.Include, name)) && !slices.Contains(f.Exclude, name)
}

// withToolFilter controls which tools ListTools exposes.
func withToolFilter(toolFilter ToolFilter) serverOption {
	return func(srv *server) {
		srv.ToolFilter = toolFilter
	}
}

// withToolPrefix namespaces this server's tools in the name the model sees,
// which keeps two servers that both publish a "search" from colliding.
//
// The prefix is used verbatim, separator included: withToolPrefix("xyz__")
// exposes the server's "search" as "xyz__search". Passing "xyz" would produce
// "xyzsearch", so carry the separator in the prefix.
//
// The prefix is presentation only. Calls are made on the server under its own
// name, and withToolFilter, withApprovalRequiredTools and withDeferredTools are
// all written against the server's own tool names, so they keep working
// unchanged when a prefix is added.
func withToolPrefix(prefix string) serverOption {
	return func(srv *server) {
		srv.ToolPrefix = prefix
	}
}

func withApprovalRequiredTools(tools ...string) serverOption {
	return func(srv *server) {
		srv.ApprovalRequiredTools = tools
	}
}

// withDeferredTools selects which exposed tools require discovery through ToolSearch.
// Omitting this option defers no tools. An empty Include or "*" includes all tools;
// Exclude takes precedence and keeps those tools directly available to the model.
// Names match the server's original names, before any prefix. "*" in Exclude
// keeps all tools directly available. withToolFilter still controls visibility.
func withDeferredTools(tools ToolFilter) serverOption {
	return func(srv *server) {
		srv.DeferredTools = &tools
	}
}

func withTransport(transport string) serverOption {
	return func(srv *server) {
		if transport == "" {
			srv.Transport = TransportSSE
		} else {
			srv.Transport = transport
		}
	}
}

// withCommand sets argv and selects the stdio transport for the private connection.
func withCommand(command string, args ...string) serverOption {
	return func(srv *server) {
		srv.Transport = TransportStdio
		srv.Command = append([]string{command}, args...)
	}
}

// withEnv adds environment variables to a stdio server's process, on top of the
// environment this process already has — the command still needs to be findable,
// so PATH and the rest are inherited rather than replaced.
//
// Values are templated against the run context the same way headers are, so a
// per-run credential reaches the server it belongs to:
//
//	mcpclient.WithEnv(map[string]string{"GITHUB_TOKEN": "{{github_token}}"})
func withEnv(env map[string]string) serverOption {
	return func(srv *server) {
		srv.Env = env
	}
}

// withCacheTTL caps a positive server TTL and supplies the TTL when the server
// returns zero or omits it. If neither TTL is positive, schemas are not cached.
func withCacheTTL(ttl time.Duration) serverOption {
	return func(srv *server) {
		srv.CacheTTL = ttl
	}
}

// withDefaultCacheScope selects the schema cache scope when the server omits
// cacheScope. Empty means private, the default. Explicit server scopes always
// take precedence. Use public only when the tool list is the same for all users.
// Legacy servers also need withCacheTTL to enable caching when they omit ttlMs.
func withDefaultCacheScope(scope string) serverOption {
	return func(srv *server) {
		srv.defaultCacheScope = scope
	}
}

// withDisableStandaloneSSE disables the post-init server→client SSE stream
// on the streamable-http transport. Enable it for servers that don't
// support the standalone GET stream (the client otherwise hangs waiting
// on a stream that never opens). Has no effect on the sse transport.
func withDisableStandaloneSSE(disable bool) serverOption {
	return func(srv *server) {
		srv.DisableStandaloneSSE = disable
	}
}

// WithSchemaCache injects a SchemaCache implementation for caching tool schemas.
// When set, ListTools() will check the cache before connecting to the MCP server.
// This enables multi-pod cache sharing when backed by Redis or similar stores.
func withSchemaCache(cache SchemaCache) serverOption {
	return func(srv *server) {
		srv.schemaCache = cache
	}
}

// withMeta configures tools/call metadata. String values (including nested maps
// and arrays) use the same {{key}} templates as headers, resolved against each
// ToolCall.RunContext at execution time. Non-string values retain their types.
// Resolved values never enter schema caches, tool descriptors, or connection keys.
func withMeta(m map[string]any) serverOption {
	return func(srv *server) {
		srv.Meta = m
	}
}

func (srv *server) GetName() string {
	return srv.Name
}

// Tool schemas are cached under what the server says about them. A 2026-07-28
// server returns ttlMs and cacheScope on every tools/list (SEP-2549): how long
// the listing stays fresh, and whether it is the same listing for everyone.
// That answers, from the server, the question this client used to have to
// guess at — whether one user's tool list may be served to another.
const (
	// CacheScopePublic shares tool schemas across users of a named MCP client.
	CacheScopePublic = "public"
	// CacheScopePrivate keeps tool schemas scoped to each requester.
	CacheScopePrivate = "private"

	cacheScopePublic  = CacheScopePublic
	cacheScopePrivate = CacheScopePrivate
)

// toolListing is one tools/list response: the schemas, and the terms the server
// offered them on.
type toolListing struct {
	Tools []*mcp.Tool

	// TTL and CacheScope are the server's cache directives. CacheScope is empty
	// from a server older than 2026-07-28, which is not the same as it saying
	// "public" — see schemaCacheKeys.
	TTL        time.Duration
	CacheScope string
}

// shareable applies the fallback only to an omitted scope. Unknown explicit
// scopes remain private. Keep the original scope in cached entries so changing
// the fallback cannot turn an old public fallback into an explicit promise.
func (srv *server) shareable(scope string) bool {
	if scope == "" {
		scope = srv.defaultCacheScope
	}
	return scope == CacheScopePublic
}

func (srv *server) ListTools(ctx context.Context, namespace string, runContext map[string]any) ([]agents.Tool, error) {
	conn, err := srv.connFor(ctx, namespace, runContext)
	if err != nil {
		return nil, err
	}

	if srv.schemaCache == nil {
		listing, err := srv.fetchToolSchemas(ctx, conn)
		if err != nil {
			return nil, err
		}
		return srv.buildLazyTools(listing.Tools, srv.Meta, conn), nil
	}

	sharedKey, privateKey := srv.schemaCacheKeys(conn)

	// Re-check shared entries against this client's fallback: another client
	// may have opted into public caching for a server that omitted its scope.
	for _, key := range []string{sharedKey, privateKey} {
		data, found, err := srv.schemaCache.Get(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("read MCP schema cache: %w", err)
		}
		var cached CachedToolEntry
		if found && json.Unmarshal(data, &cached) == nil && !cached.ExpiresAt.IsZero() && !cached.expired() {
			if key == sharedKey && !srv.shareable(cached.CacheScope) {
				continue
			}
			return srv.buildLazyTools(cached.Tools, srv.Meta, conn), nil
		}
	}

	listing, err := srv.fetchToolSchemas(ctx, conn)
	if err != nil {
		return nil, err
	}

	// A non-positive server TTL may use an explicit local TTL override.
	// Without one, bypass caching instead of storing an unbounded entry.
	if listing.TTL <= 0 && srv.CacheTTL <= 0 {
		return srv.buildLazyTools(listing.Tools, srv.Meta, conn), nil
	}

	key := privateKey
	if srv.shareable(listing.CacheScope) {
		key = sharedKey
	}
	entry := &CachedToolEntry{
		Tools:      listing.Tools,
		CacheScope: listing.CacheScope,
	}
	ttl := srv.cacheTTL(listing)
	if ttl > 0 {
		entry.ExpiresAt = time.Now().Add(ttl)
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return nil, fmt.Errorf("encode MCP schema cache: %w", err)
	}
	if err = srv.schemaCache.Set(ctx, key, data, ttl); err != nil {
		return nil, fmt.Errorf("write MCP schema cache: %w", err)
	}

	return srv.buildLazyTools(listing.Tools, srv.Meta, conn), nil
}

// cacheTTL is how long a listing may be held: what the server asked for,
// bounded by what the caller allowed, the way a caching proxy's own max-age
// bounds an upstream's. A non-positive server TTL uses the configured local
// TTL. Responses bypass caching if neither TTL is positive, including legacy
// responses without cache directives.
func (srv *server) cacheTTL(listing toolListing) time.Duration {
	if listing.TTL > 0 && srv.CacheTTL > 0 {
		return min(listing.TTL, srv.CacheTTL)
	}
	if listing.TTL > 0 {
		return listing.TTL
	}
	return srv.CacheTTL
}

// CallToolDirect runs one tool against this server without listing tools first,
// reusing a pooled connection. Listing instead would cost a fresh MCP session
// and a tools/list before every single call, since schemas are only cached when
// a SchemaCache is injected.
//
// It takes the tool rather than looking one up by name. A durable runtime's
// workflow already holds the tool as serialized data and hands it back here, so
// there is nothing to resolve: tool.Name is the name the server knows, already
// free of any prefix the model-facing name carries. The current filter is checked
// again because configuration edits can revoke a previously listed tool.
func (srv *server) CallToolDirect(ctx context.Context, runContext map[string]any, tool *agents.BaseTool, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	// The call carries the execution namespace required to resolve credentials.
	if params == nil {
		return nil, errors.New("mcp: tool call is required")
	}

	if tool == nil || tool.Name == "" {
		return nil, fmt.Errorf("mcp: cannot call %q without the tool it names", params.Name)
	}
	if !srv.ToolFilter.allows(tool.Name) {
		return nil, fmt.Errorf("mcp: tool %q is excluded by the current filter for server %q", tool.Name, srv.Name)
	}

	// The run context comes in as its own argument on this path — a durable
	// runtime's workflow holds only a serialized tool definition and calls back
	// here to execute. Put it on the call so anything downstream reads it in
	// the one place it reads it everywhere else.
	if params.RunContext == nil {
		params.RunContext = runContext
	}

	conn, err := srv.connFor(ctx, params.Namespace, runContext)
	if err != nil {
		return nil, err
	}

	lazy := &LazyMcpTool{
		BaseTool: tool,
		conn:     conn,
		meta:     srv.Meta,
	}
	return lazy.Execute(ctx, params)
}

// InvalidateToolCache removes cached tool schemas for this MCP server.
func (srv *server) InvalidateToolCache(ctx context.Context, namespace string, runContext map[string]any) error {
	if srv.schemaCache == nil {
		return nil
	}
	conn, err := srv.connFor(ctx, namespace, runContext)
	if err != nil {
		return err
	}
	sharedKey, privateKey := srv.schemaCacheKeys(conn)
	return errors.Join(srv.schemaCache.Delete(ctx, sharedKey), srv.schemaCache.Delete(ctx, privateKey))
}

// connFor builds the description of this server that the transport and the pool
// both work from. Headers and env are resolved against the run context here, so
// a per-run credential reaches the server whichever transport carries it, and
// so does the token source — see credentialsFor for why that matters under a
// durable runtime.
func (srv *server) connFor(ctx context.Context, namespace string, runContext map[string]any) (serverConn, error) {
	tokens, principal, err := srv.credentialsFor(ctx, namespace, runContext)
	if err != nil {
		return serverConn{}, err
	}

	// Scope sessions by configuration owner without retaining configuration versions.
	return serverConn{
		Name:                 srv.Name,
		Namespace:            srv.namespace,
		Transport:            srv.Transport,
		Endpoint:             srv.Endpoint,
		Headers:              srv.resolveHeaders(runContext),
		Command:              srv.Command,
		Env:                  resolveTemplates(srv.Env, runContext),
		TokenSource:          tokens,
		Principal:            principal,
		AuthorizationKey:     authorizationDigest(srv.authorization),
		DisableStandaloneSSE: srv.DisableStandaloneSSE,
	}, nil
}

// resolveHeaders resolves template variables in headers using the runContext.
func (srv *server) resolveHeaders(runContext map[string]any) map[string]string {
	return resolveTemplates(srv.Headers, runContext)
}

// resolveTemplates fills a set of configured values in from the run context.
func resolveTemplates(values map[string]string, runContext map[string]any) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for k, v := range values {
		out[k] = utils.TryAndParseAsTemplate(v, runContext)
	}
	return out
}

// schemaCacheKeys are the two keys a listing may live under.
//
// A listing the server called public is the same for everyone, so it goes under
// a key that names no requester and every run reads it. Anything else goes
// under a key that does name one: the tool list a server shows an admin is not
// the one it shows everybody, and one user's must never be served to another.
//
// An omitted scope is private unless withDefaultCacheScope explicitly opts
// into public sharing for a server known to expose the same tools to everyone.
//
// The definition's namespace and name identify a server independently of config
// edits. A public listing is shared only within that configuration's scope.
// Existing schemas remain cached until their TTL expires or they are invalidated.
//
// What is cached is the server's listing as it gave it, so no client's own
// view of it belongs in the key: ToolFilter and ToolPrefix are both applied by
// buildLazyTools, on the way out, to a hit and a miss alike. Putting either
// here only stored the same schemas twice.
func (srv *server) schemaCacheKeys(conn serverConn) (shared, private string) {
	shared = fmt.Sprintf("mcp:schema:%q:%q", srv.namespace, srv.Name)
	return shared, shared + "|" + conn.requesterKey()
}

// fetchToolSchemas connects to the MCP server, fetches tool schemas, and closes
// the connection.
func (srv *server) fetchToolSchemas(ctx context.Context, conn serverConn) (toolListing, error) {
	session, err := connect(ctx, conn)
	if err != nil {
		return toolListing{}, err
	}
	// Close the connection — we only needed the schemas. Actual tool execution
	// will use the connection pool.
	defer session.Close()

	res, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		return toolListing{}, err
	}

	return toolListing{
		Tools:      res.Tools,
		TTL:        time.Duration(res.GetTTLMs()) * time.Millisecond,
		CacheScope: res.GetCacheScope(),
	}, nil
}

// buildLazyTools converts mcp.Tool schemas into LazyMcpTool instances, applying
// tool filters, approval flags, and deferred flags against the server's original
// names before adding the model-facing prefix.
func (srv *server) buildLazyTools(tools []*mcp.Tool, meta mcp.Meta, conn serverConn) []agents.Tool {
	var result []agents.Tool
	for _, tool := range tools {
		if !srv.ToolFilter.allows(tool.Name) {
			continue
		}

		// Deferral changes initial model availability, without removing an exposed tool.
		requiresApproval := slices.Contains(srv.ApprovalRequiredTools, tool.Name)
		deferred := srv.isDeferred(tool.Name)

		lazy := NewLazyMcpTool(tool, conn, meta, requiresApproval, deferred, srv.ToolPrefix)
		result = append(result, lazy)
	}
	return result
}

// isDeferred applies the configured selection to the server's original tool name.
func (srv *server) isDeferred(name string) bool {
	// The default client exposes every allowed tool directly.
	filter := srv.DeferredTools
	if filter == nil {
		return false
	}

	// Exclusions keep tools directly available even when they also match Include.
	if slices.Contains(filter.Exclude, name) || slices.Contains(filter.Exclude, "*") {
		return false
	}

	// An explicit empty selection defers all allowed tools, as does the wildcard.
	return len(filter.Include) == 0 || slices.Contains(filter.Include, name) || slices.Contains(filter.Include, "*")
}
