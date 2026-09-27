# Persisted MCP credentials

MCP connection definitions and tokens have separate stores:

- `MCPServerConfigStore` holds server endpoints and OAuth application settings.
- `CredentialStore` holds tokens under **namespace + credential key**. The key,
  `OAuthCredentialKey(connector)`, binds the definition's owner (global or the
  user's own), its server name, and the OAuth client ID and token URL. Treat it as
  an opaque string; pass it to `CredentialStore.Delete` to disconnect an account.

The client passes the current server configuration to the credential provider.
The HTTP handler reads OAuth settings from the same config store by server name
and authenticated namespace. There is no registration map to keep synchronized,
and connecting an account does not require listing tools first.

```go
configs, err := mcpclient.NewFileStore("./data/mcp-config.json")
if err != nil {
    return err
}
configs = configs.WithMCPServerConfig([]mcpclient.ServerConfig{{
    Name: "gmail",
    Endpoint: "https://gmailmcp.googleapis.com/mcp/v1",
    Transport: mcpclient.TransportStreamableHTTP,
    ToolPrefix: "gmail__",
    Authorization: &mcpclient.OAuthConfig{
        ClientID: os.Getenv("GMAIL_CLIENT_ID"),
        ClientSecret: os.Getenv("GMAIL_CLIENT_SECRET"),
        RedirectURL: "http://localhost:8070/api/agui/mcp/gmail/callback",
        AuthURL: "https://accounts.google.com/o/oauth2/v2/auth",
        TokenURL: "https://oauth2.googleapis.com/token",
        Scopes: []string{"https://www.googleapis.com/auth/gmail.readonly"},
        AuthCodeParams: map[string]string{"prompt": "consent"},
    },
}})

// Share token refresh coordination between agent execution and browser callbacks.
tokens, err := mcpclient.NewFileCredentialStore("./data/mcp-credentials")
if err != nil {
    return err
}
defer tokens.Close()
provider, err := mcpclient.NewOAuthCredentialProvider(mcpclient.OAuthCredentialProviderConfig{
    Store: tokens,
})
if err != nil {
    return err
}
client := mcpclient.NewClient(configs).WithCredentials(provider)
```

Give `client` to the agent's `MCPClient` option, then mount the config store:

```go
handler := web.Handler(registry,
    agui.WithNamespaceResolver(authenticatedNamespace),
    agui.WithMCPStore(configs, mcpclient.WithOAuth(provider)),
)
```

Use the same authenticated namespace for agent runs and HTTP requests. For the
standalone handler, use `NewHandler(configs, WithNamespaceResolver(...),
WithOAuth(provider))` and mount it with `http.StripPrefix`.

Visit `/api/agui/mcp/gmail/connect`. The handler resolves Gmail's OAuth config,
creates expiring browser-bound state and an S256 PKCE challenge, then redirects
to consent. The callback checks the cookie, namespace, server, and one-use state,
exchanges the code, and persists refreshable credentials. Subsequent agent runs
resolve those tokens automatically. Missing credentials become an authentication
connector status before opening an MCP connection or using cached tools.

Register the full callback URL in the OAuth application. The sample in
`samples/new/main.go` reads `GMAIL_CLIENT_ID`, `GMAIL_CLIENT_SECRET`, and optionally
`SAMPLE_NAMESPACE`; its default namespace is `default`.

## Persistence and deployment

File credential persistence uses private directories/files and atomic writes.
Tokens are plaintext on disk: place the directory on appropriately protected
storage. The provider reloads tokens per request, refreshes expired credentials,
and saves rotated refresh tokens before returning them. One shared provider
coordinates refresh and callback writes in one process. Multi-process deployments
need a credential store/refresh coordinator that handles concurrent rotation.

OAuth state is process-local and expires after ten minutes; connect and callback
must reach the same handler instance. A flow uses the OAuth configuration captured
when it started. Future flows use the latest config in the store. A grant is
never reused by a different definition that takes the same name, and changing a
definition's client ID or token URL requires the user to reconnect; the old record
is left in the credential store for the application to clean up. Other edits keep
the grant. OAuth URLs require HTTPS. Only developer-owned
global definitions may use loopback HTTP URLs for local development.

Namespace-owned definitions must use public addresses for the MCP endpoint and the
token URL, the two URLs this process requests. The authorization and callback URLs
are browser navigations and follow the HTTPS-or-loopback rule, so a local callback
works during development. Token exchange and refresh validate DNS results and
dial vetted public IPs directly, bypassing environment proxies. Redirects must
stay on the same origin. A provider's custom `HTTPClient` applies only to global
definitions; user definitions always use the protected transport. See the
[configuration security rules](CATALOG.md#connector-http-api) for details.

## Custom credential providers

Implement `CredentialProvider.Resolve(ctx, namespace, connector, runContext)` and
return a token source and stable principal. Set `UseCredentials: true` on the
relevant global `ServerConfig` (user-owned definitions cannot set it, so a user's
endpoint never receives application credentials), and call `client.WithCredentials(provider)` before sharing
the client. Custom providers need no OAuth routes. Credentials stay out of agent
run state and durable histories; `Connector.Authorization` contains the current
OAuth settings when present.
