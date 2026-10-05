# MCP configuration stores

All MCP servers are defined by `ServerConfig`. The client accepts one or more
stores; there is no separate constructor for code-defined servers.

```go
store, err := mcpclient.NewFileStore("./data/mcp-config.json")
if err != nil {
    return err
}

// Add developer-owned definitions without modifying the file or the original view.
store = store.WithMCPServerConfig([]mcpclient.ServerConfig{{
    Name:       "docs",
    Endpoint:   "https://example.com/mcp",
    Transport:  mcpclient.TransportStreamableHTTP,
    ToolPrefix: "docs__",
}})
client := mcpclient.NewClient(store)

agent := hastekit.MustNewAgent(&hastekit.AgentConfig{
    Name: "Assistant", LLM: model, MCPClient: client,
})
```

Use `NewMemoryStore()` for process-local persistence. `NewStore(databaseStore)`
adds inline configuration support to a custom database source. Chaining
`WithMCPServerConfig` returns a new view each time; inline maps and slices are
copied, while the backing persistence is shared. Inline values override the same
namespace/name in that view and are read-only through HTTP.

## Store contract and ownership

```go
type MCPServerConfigStore interface {
    ListServerConfigs(context.Context, string, map[string]any) ([]ServerConfig, error)
}
```

The arguments are context, authoritative namespace, and run context. Run context
uses the SDK's existing `map[string]any` type. Return global records plus records
owned by the requested namespace. An empty namespace lists globals only.

- `ServerConfig.Namespace == ""` means developer-owned/global.
- Global definitions take precedence over user definitions with the same name.
- `NewClient(storeA, storeB)` reads both sources. Duplicate global definitions are
  errors; precedence between different sources is not implicit. Invalid or
  duplicate user-owned records fail only their own connector (see below).
- `Put(ctx, namespace, config)` and `Delete(ctx, namespace, name)` form the optional
  `MutableMCPServerConfigStore` interface. Put uses its namespace argument rather
  than trusting `config.Namespace`.
- File stores reload on each listing and atomically replace private files on
  writes. Share a file store instance for writes in one process. For concurrent
  writes across processes, implement a transactional database store.

The file format is a JSON array of `ServerConfig` values:

```json
[
  {"name":"docs","endpoint":"https://example.com/mcp","transport":"streamable-http","toolPrefix":"docs__"},
  {"name":"notes","namespace":"alice","endpoint":"https://notes.example/mcp","transport":"streamable-http"}
]
```

## Discovery and selection

```go
statuses, tools, err := client.ListTools(ctx, namespace, runContext)

// The optional selection narrows availability for this run.
statuses, tools, err = client.ListTools(ctx, namespace, runContext, agents.MCPSelection{
    Disable: []string{"archive"},
    Tools: map[string]agents.MCPToolSelection{
        "notes": {Include: []string{"search"}, Exclude: []string{"delete"}},
    },
})
```

Agents pass `AgentInput.MCP` automatically. AG-UI accepts the same selection in
`forwardedProps.mcp`. Global servers are always enabled and cannot be disabled.
Namespace-owned servers are enabled by default, including newly added servers,
unless their names appear in Disable, the same rule as skills. Tool selection narrows the configured filter; it cannot restore
a tool excluded by `ServerConfig.ToolFilter`. Names are the original server tool
names before prefixes. An empty Include selects all tools and Exclude wins.

An individual connection/authentication failure becomes a connector status so
other connectors remain usable. Store failures, invalid global definitions, and
tool-name collisions between globals return errors: the developer owns them. A
user-owned definition that is invalid, duplicated, or exposes a tool name already
taken fails only its own connector, reported as a status, so one bad record or an
upstream tool rename never locks its owner out. Globals are listed first so they
always keep their tool names; within each group, connectors are ordered by name.
The HTTP handler still lists unusable user records, with an `error` explaining
why, and lets their owner delete or replace them. The agent passes statuses into prompt dependencies.
Disabled connectors contribute neither tools nor statuses and do not connect or
resolve credentials. Prompt rendering uses the returned statuses directly; when
all connectors are disabled, there are no statuses and no connector section.

Tool calls read the current store configuration. Editing a definition affects
subsequent calls, including calls listed before the edit. The server name comes from `BaseTool.MCPServerName`; the authoritative namespace
comes from `ToolCall.Namespace`. Tool metadata remains separate from routing. Temporal activities and Restate steps resolve configuration and
credentials at execution time; workflow history contains only descriptors and
statuses, not connection secrets. The client does not retain configuration revisions or cache constructed server
objects. Connection pooling and namespace-scoped schema caching still apply;
existing schemas remain cached until their TTL expires or they are invalidated.

## Connector HTTP API

```go
handler := mcpclient.NewHandler(store,
    mcpclient.WithNamespaceResolver(authenticatedNamespace),
    mcpclient.WithOAuth(oauthProvider), // optional; share with the client
)
mux.Handle("/mcp/", http.StripPrefix("/mcp", handler))
```

| Route | Behavior |
| --- | --- |
| `GET /mcp/` | Visible connector summaries, without secrets |
| `PUT /mcp/{server}` | Create or replace the authenticated user's config |
| `DELETE /mcp/{server}` | Remove the authenticated user's config |
| `GET /mcp/{server}/connect` | Start OAuth using the current store definition |
| `GET /mcp/{server}/callback` | Validate state/PKCE, exchange code, persist tokens |

OAuth routes require `WithOAuth`. All routes require a trusted namespace resolver;
without one, requests are forbidden. Global and inline configurations are
read-only. User HTTP writes allow remote MCP transports, not local process
commands or environment variables. Read-only custom stores support listing but
reject writes. Applications own user authentication.

Namespace-owned definitions cannot contain `{{` template delimiters, including
in headers or nested metadata. Registration rejects them before persistence;
file loading, inline definitions, and client discovery validate the same rule.
Only developer-owned global definitions can resolve templates against run context.
Literal user-supplied headers and metadata remain supported.

User endpoints must use public HTTP(S) addresses. Private, loopback, link-local,
multicast, and special-use IPs are rejected, including cloud metadata addresses
and IPv4-mapped IPv6. Literal addresses and local hostnames are checked at
registration. DNS names are checked when connecting: every answer must be public,
and the socket connects directly to a vetted IP, preventing DNS rebinding.
These requests bypass environment proxies. MCP requests, redirects, and SSE
message endpoints must stay on the configured origin.

The same public-address policy applies to a user definition's OAuth token URL.
Authorization and callback URLs are only browser redirects, so they need HTTPS
(or loopback HTTP for local development) but not a public address. Code exchanges and
token refreshes use the protected transport and cannot redirect to another
origin. A provider's custom `HTTPClient` applies only to global definitions.
Global definitions remain trusted application configuration and can use local
services, templates, and custom OAuth transports. Additional deployment-specific
egress restrictions belong in the application's network policy.

For the SDK web handler, use:

```go
web.Handler(registry,
    agui.WithNamespaceResolver(authenticatedNamespace),
    agui.WithMCPStore(store, mcpclient.WithOAuth(oauthProvider)),
)
```

These routes are mounted under `/api/agui/mcp`. Management routes pass nil run
context to stores; a custom store should return its namespace's catalog in that
case. Register OAuth redirect URLs using the complete callback path.

## Credentials and caching

`ServerConfig.Authorization` holds OAuth application settings. Share one
`OAuthCredentialProvider` between client and handler:

```go
client := mcpclient.NewClient(store).WithCredentials(provider).WithSchemaCache(cache)
```

For a custom `CredentialProvider`, set `UseCredentials: true` on global configs
that need it; user-owned configs cannot set it. Public definitions do not use the
provider. OAuth tokens are keyed by the execution namespace and
`OAuthCredentialKey`, which binds the definition's owner, name, and OAuth client. OAuth needs no provider-side registration;
HTTP authorization can start before the first agent run. See [OAuth setup](OAUTH.md).

Set `CacheTTLSeconds` and `DefaultCacheScope` in each config. Explicit server
cache scope takes precedence; absent scopes default to private unless configured
otherwise. Use public scope only when all users see the same tool list.
