# MCP configuration stores

All MCP servers are defined by `ServerConfig`. The client takes two stores: a
built-in store of the developer's servers and a user store of each namespace's
own.

```go
store, err := mcpclient.NewFileStore("./data/mcp-config.json") // users' connectors
if err != nil {
    return err
}

// Developer-owned definitions, kept out of the file.
builtins := mcpclient.NewInlineStore(mcpclient.ServerConfig{
    Name:       "docs",
    Endpoint:   "https://example.com/mcp",
    Transport:  mcpclient.TransportStreamableHTTP,
    ToolPrefix: "docs__",
})
client := mcpclient.NewClient(builtins, mcpclient.WithUserStore(store))

agent := hastekit.MustNewAgent(&hastekit.AgentConfig{
    Name: "Assistant", LLM: model, MCPClient: client,
})
```

`NewInlineStore(configs...)` copies its definitions and serves them for every
namespace: each must leave `Namespace` empty, and an invalid one is reported
when the store is listed. It never accepts writes. Use `NewMemoryStore()` for
process-local persistence and `NewStore(databaseStore)` to validate writes
before they reach a custom database source.

## Store contract and ownership

```go
type Store interface {
    ListServerConfigs(context.Context, string, map[string]any) ([]ServerConfig, error)
}
```

The arguments are context, namespace, and run context. Run context uses the
SDK's existing `map[string]any` type. Return the records of the requested
namespace only; the empty namespace is a key like any other. A store does not
know whether its servers are global or a user's.

- `NewClient(builtIn, WithUserStore(user))` reads the built-in store under the empty namespace
  and the user store under the run's, and sets each record's `Namespace` from
  the store it came from: empty for a built-in (developer-owned, global), the
  namespace for a user's own. So any store can fill either slot, one store can
  fill both (`NewClient(store, WithUserStore(store))` reads the file's records without a
  namespace as built-ins), and either may be nil.
- Built-ins take precedence over user definitions with the same name. An invalid
  or duplicate built-in fails the listing; invalid or duplicate user-owned
  records fail only their own connector (see below).
- `Filter(store, servers)` lists only the servers a store's map names, each
  narrowed by its `ToolFilter`, so an agent can take part of a store. Filter
  either of the client's stores, or both.

  ```go
  client := NewClient(
      Filter(builtins, map[string]ToolFilter{
          "docs":   {},                                                // all its tools
          "github": {Include: []string{"get_issue", "search_issues"}}, // only these
      }),
      WithUserStore(Filter(users, map[string]ToolFilter{
          "gmail": {Include: []string{"search"}}, // each user's own gmail
      })),
  )
  ```

  Users' servers a filter on the user store does not name are dropped. A kept
  built-in wins over a user's server with the same name, as usual.

  Tool names are original server names, before any prefix. A tool filter narrows
  the server's own `ServerConfig.ToolFilter` and cannot restore a tool it
  excludes; per-run `MCPSelection.Tools` narrows further. The filter also applies
  when tools are called, so a server or tool left out is neither listed nor
  callable. Unknown server names are ignored. A built-in left out does not shadow
  a kept user server with the same name; OAuth grants stay bound to the built-in
  definition. The filtered store is read-only: give `NewHandler` the unfiltered
  user store, and the unfiltered built-in store with `WithBuiltInStore`, so users
  manage their servers there and every built-in name stays reserved.
- `Put(ctx, namespace, config)` and `Delete(ctx, namespace, name)` form the optional
  `MutableStore` interface. Put uses its namespace argument rather
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

// The optional selection narrows the namespace's own servers for this run.
statuses, tools, err = client.ListTools(ctx, namespace, runContext, agents.MCPSelection{
    Disable: []string{"archive"},
    Tools: map[string]agents.MCPToolSelection{
        "notes": {Include: []string{"search"}, Exclude: []string{"delete"}},
    },
})
```

Agents pass `AgentInput.MCP` automatically. AG-UI accepts the same selection in
`forwardedProps.mcp`. Global servers are mandatory: a run can neither disable
them nor narrow their tools, so their names are ignored in both `Disable` and
`Tools`, the same rule as global skills. Narrow a global server for an agent with
`ServerConfig.ToolFilter` or `Filter` instead. Namespace-owned servers are
enabled by default, including newly added servers, unless their names appear in
Disable. Tool selection narrows a namespace server's configured filter; it cannot
restore a tool excluded by `ServerConfig.ToolFilter`. Names are the original
server tool names before prefixes. An empty Include selects all tools and Exclude wins.

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
    mcpclient.WithBuiltInStore(builtins),
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
without one, requests are forbidden. `NewHandler(store, ...)` takes the
client's user store, and its built-in store through `WithBuiltInStore`. Writes go
to the user store; the built-in store's servers are listed, reserve their names, and serve OAuth connect, but are never written.
A user store that cannot be written lists the user's servers read-only and
rejects writes. User HTTP writes allow remote MCP transports, not local process
commands or environment variables. Applications own user authentication.

Namespace-owned definitions cannot contain `{{` template delimiters, including
in headers or nested metadata. Registration rejects them before persistence;
file loading and client discovery validate the same rule.
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
    agui.WithMCPStore(store, mcpclient.WithBuiltInStore(builtins), mcpclient.WithOAuth(oauthProvider)),
)
```

These routes are mounted under `/api/agui/mcp`. Management routes pass nil run
context to stores; a custom store should return its namespace's catalog in that
case. Register OAuth redirect URLs using the complete callback path.

## Credentials and caching

`ServerConfig.Authorization` holds OAuth application settings. Share one
`OAuthCredentialProvider` between client and handler:

```go
client := mcpclient.NewClient(builtins,
    mcpclient.WithUserStore(store),
    mcpclient.WithCredentials(provider),
    mcpclient.WithSchemaCache(cache),
)
```

For a custom `CredentialProvider`, set `UseCredentials: true` on global configs
that need it; user-owned configs cannot set it. Public definitions do not use the
provider. OAuth tokens are keyed by the execution namespace and
`OAuthCredentialKey`, which binds the definition's owner, name, and OAuth client. OAuth needs no provider-side registration;
HTTP authorization can start before the first agent run. See [OAuth setup](OAUTH.md).
A definition written in code, such as `NewInlineStore`'s, can instead carry a
`CredentialsProvider` of its own for that server alone; it is never JSON, stores
refuse to save one, and user-owned definitions cannot set one. See [a server's
own credentials provider](OAUTH.md#a-servers-own-credentials-provider).

Set `CacheTTLSeconds` and `DefaultCacheScope` in each config. Explicit server
cache scope takes precedence; absent scopes default to private unless configured
otherwise. Use public scope only when all users see the same tool list.
