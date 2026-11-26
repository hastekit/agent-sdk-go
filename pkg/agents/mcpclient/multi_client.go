package mcpclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
)

// Client manages MCP servers from two stores. It mirrors skills.Client.
//
// The built-in store holds the developer's servers: global, listed for every
// namespace, always on, and winning over a user's server with the same name;
// an invalid or duplicate one fails the listing. The user store, given with
// WithUserStore, holds each namespace's own servers, which a run may turn off
// or narrow (agents.MCPSelection); an invalid, duplicate, or colliding one
// fails only its own connector. Stores do not know which role they play: the
// client reads the built-in store under the empty namespace and the user store
// under the run's, so any store can fill either, and one store can fill both:
//
//	builtIns := mcpclient.NewInlineStore(mcpclient.ServerConfig{Name: "docs", Endpoint: "https://docs.example/mcp"})
//	users, err := mcpclient.NewFileStore("./data/mcp.json")
//	client := mcpclient.NewClient(builtIns, mcpclient.WithUserStore(users), mcpclient.WithCredentials(provider))
type Client struct {
	builtIn     Store
	user        Store
	credentials CredentialProvider
	cache       SchemaCache
}

// ClientOption configures NewClient.
type ClientOption func(*Client)

// WithUserStore reads each namespace's own servers from store.
func WithUserStore(store Store) ClientOption {
	return func(c *Client) { c.user = store }
}

// WithCredentials supplies shared credential resolution; OAuth settings live in ServerConfig.
func WithCredentials(provider CredentialProvider) ClientOption {
	return func(c *Client) { c.credentials = provider }
}

// WithSchemaCache supplies optional shared discovery caching.
func WithSchemaCache(cache SchemaCache) ClientOption {
	return func(c *Client) { c.cache = cache }
}

// NewClient reads its stores at discovery and execution time, so servers added
// at run time appear on the next run. builtIn may be nil, for a client of
// users' servers only. The client is safe to share across agents.
func NewClient(builtIn Store, options ...ClientOption) *Client {
	client := &Client{builtIn: builtIn}
	for _, option := range options {
		if option != nil {
			option(client)
		}
	}
	return client
}

// catalog is one resolved view of the definitions visible to a namespace.
type catalog struct {
	// configs holds usable definitions by name; a built-in shadows a same-named
	// user definition. Each carries the namespace that owns it: empty for a
	// built-in, the namespace for a user's own.
	configs map[string]ServerConfig
	// invalid holds user-owned definitions that cannot be used, by name. They are reported as
	// failed connectors rather than failing the owner's whole catalog, and remain deletable.
	invalid map[string]error
}

// readStore lists one store under namespace, setting each definition's
// Namespace to it, so ownership follows the store a definition came from. A
// definition that names another namespace is an application fault.
func readStore(ctx context.Context, store Store, namespace string, runContext map[string]any) ([]ServerConfig, error) {
	if store == nil {
		return nil, ctx.Err()
	}
	listed, err := store.ListServerConfigs(ctx, namespace, runContext)
	if err != nil {
		return nil, fmt.Errorf("list MCP server configurations: %w", err)
	}
	for i := range listed {
		if listed[i].Namespace != "" && listed[i].Namespace != namespace {
			return nil, errors.New("MCP store returned a server from another namespace")
		}
		listed[i].Namespace = namespace
	}
	return listed, nil
}

// listServerConfigs resolves one visible catalog before doing network I/O, enforcing precedence.
// Store failures and invalid built-ins are errors: they are application faults.
// Invalid or duplicate user-owned records are isolated so one bad record cannot lock its owner out.
func (c *Client) listServerConfigs(ctx context.Context, namespace string, runContext map[string]any) (catalog, error) {
	if err := ctx.Err(); err != nil {
		return catalog{}, err
	}
	builtIns, err := readStore(ctx, c.builtIn, "", runContext)
	if err != nil {
		return catalog{}, err
	}
	result := catalog{configs: map[string]ServerConfig{}, invalid: map[string]error{}}
	for _, config := range builtIns {
		if err := validateServerConfig(config); err != nil {
			return catalog{}, err
		}
		if _, exists := result.configs[config.Name]; exists {
			return catalog{}, fmt.Errorf("duplicate global MCP server %q", config.Name)
		}
		copy, err := cloneServerConfig(config)
		if err != nil {
			return catalog{}, err
		}
		result.configs[config.Name] = copy
	}
	if namespace == "" {
		return result, nil
	}

	users, err := readStore(ctx, c.user, namespace, runContext)
	if err != nil {
		return catalog{}, err
	}
	own := map[string]ServerConfig{}
	for _, config := range users {
		// Built-ins win, including over invalid user records.
		if _, global := result.configs[config.Name]; global {
			continue
		}
		// A name recorded twice is ambiguous, whichever copy is valid.
		if _, exists := own[config.Name]; exists || result.invalid[config.Name] != nil {
			delete(own, config.Name)
			result.invalid[config.Name] = fmt.Errorf("duplicate MCP server %q in one scope", config.Name)
			continue
		}
		if err := validateServerConfig(config); err != nil {
			result.invalid[config.Name] = err
			continue
		}
		copy, err := cloneServerConfig(config)
		if err != nil {
			result.invalid[config.Name] = err
			continue
		}
		own[config.Name] = copy
	}
	maps.Copy(result.configs, own)
	return result, nil
}

// ListedServer is one of a client's servers, as a UI offering them shows it.
type ListedServer struct {
	Name string `json:"name"`
	// AlwaysOn reports a server no run can turn off or narrow: a global, or
	// one the client pins.
	AlwaysOn bool `json:"always_on"`
}

// ServerLister is an MCP client that can list the servers it has for a
// namespace without connecting to any. Client implements it.
type ServerLister interface {
	ListServers(ctx context.Context, namespace string, runContext map[string]any) ([]ListedServer, error)
}

// ListServers lists the servers the client has for namespace — the same ones
// ListTools would connect to with no selection, unusable ones included —
// without connecting: globals first, always on, then the namespace's own,
// each group by name.
func (c *Client) ListServers(ctx context.Context, namespace string, runContext map[string]any) ([]ListedServer, error) {
	resolved, err := c.listServerConfigs(ctx, namespace, runContext)
	if err != nil {
		return nil, err
	}
	var globals, users []ListedServer
	for name, config := range resolved.configs {
		if config.Namespace == "" {
			globals = append(globals, ListedServer{Name: name, AlwaysOn: true})
		} else {
			users = append(users, ListedServer{Name: name})
		}
	}
	for name := range resolved.invalid {
		users = append(users, ListedServer{Name: name})
	}
	byName := func(servers []ListedServer) {
		sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
	}
	byName(globals)
	byName(users)
	return append(append([]ListedServer{}, globals...), users...), nil
}

var _ ServerLister = (*Client)(nil)

// build configures a lightweight server from the current store definition.
// Network sessions and tool schemas are reused by their existing pool and cache.
func (c *Client) build(ctx context.Context, config ServerConfig) (*server, error) {
	// Build the private transport from one resolved definition.
	options := []serverOption{
		withTransport(config.Transport),
		withHeaders(config.Headers),
		withEnv(config.Env),
		withMeta(config.Meta),
		withToolPrefix(config.ToolPrefix),
		withToolFilter(config.ToolFilter),
		withApprovalRequiredTools(config.ApprovalRequiredTools...),
		withCacheTTL(time.Duration(config.CacheTTLSeconds) * time.Second),
		withDefaultCacheScope(config.DefaultCacheScope),
		withSchemaCache(c.cache),
		withDisableStandaloneSSE(config.DisableStandaloneSSE),
		func(server *server) {
			server.namespace = config.Namespace
			server.authorization = config.Authorization
		},
	}
	if config.Transport == TransportStdio {
		options = append(options, withCommand(config.Command[0], config.Command[1:]...))
	}
	if config.DeferredTools != nil {
		options = append(options, withDeferredTools(*config.DeferredTools))
	}

	// A server with a provider of its own uses it; only definitions that request
	// credentials otherwise use the shared provider.
	switch {
	case config.CredentialsProvider != nil:
		options = append(options, withCredentials(config.CredentialsProvider))
	case config.UseCredentials || config.Authorization != nil:
		if c.credentials == nil {
			return nil, errors.New("MCP server requests credentials but no provider is configured")
		}
		options = append(options, withCredentials(c.credentials))
	}

	// Construction keeps no cached copy of the definition.
	return newServer(ctx, config.Name, config.Endpoint, options...)
}

// ListTools returns statuses and tools only for connectors enabled for this run.
// Store/identity/collision failures abort listing; an individual connector failure does not.
func (c *Client) ListTools(ctx context.Context, namespace string, runContext map[string]any, selection ...agents.MCPSelection) ([]agents.ConnectorStatus, []agents.Tool, error) {
	// Selection is optional for direct callers and explicit for agent runs.
	var selected agents.MCPSelection
	if len(selection) > 1 {
		return nil, nil, errors.New("MCP listing accepts at most one selection")
	}
	if len(selection) == 1 {
		selected = selection[0]
	}
	resolved, err := c.listServerConfigs(ctx, namespace, runContext)
	if err != nil {
		return nil, nil, err
	}

	// Globals list first so their tool names always win; each group is ordered by name,
	// which keeps prompts and durable activity results deterministic.
	var globalNames, userNames []string
	for name, config := range resolved.configs {
		if config.Namespace == "" {
			globalNames = append(globalNames, name)
		} else {
			userNames = append(userNames, name)
		}
	}
	for name := range resolved.invalid {
		userNames = append(userNames, name)
	}
	sort.Strings(globalNames)
	sort.Strings(userNames)
	names := append(globalNames, userNames...)
	statuses := make([]agents.ConnectorStatus, 0, len(names))
	tools := []agents.Tool{}
	exposed := map[string]bool{}

	// Disabled servers need no credentials, connection, or tool listing.
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		config, valid := resolved.configs[name]
		// Global connectors are mandatory; namespace connectors are enabled unless disabled.
		if (!valid || config.Namespace != "") && slices.Contains(selected.Disable, name) {
			continue
		}
		if !valid {
			err := fmt.Errorf("invalid MCP server configuration: %w", resolved.invalid[name])
			logUnavailable(ctx, namespace, name, err)
			statuses = append(statuses, agents.FailedConnectorStatus(name, err))
			continue
		}

		// A global misconfiguration fails the run; a user's own only fails that connector.
		server, err := c.build(ctx, config)
		if err != nil {
			logUnavailable(ctx, namespace, name, err)
			if config.Namespace != "" {
				statuses = append(statuses, agents.FailedConnectorStatus(name, err))
				continue
			}
			return nil, nil, fmt.Errorf("configure MCP server %q: %w", name, err)
		}

		// Preserve per-connector failures as status while other connectors continue.
		listed, err := server.ListTools(ctx, namespace, runContext)
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if err != nil {
			logUnavailable(ctx, namespace, name, err)
			statuses = append(statuses, agents.FailedConnectorStatus(name, err))
			continue
		}

		// Narrow configured tools and retain the server name for execution.
		var bound []agents.Tool
		var conflict string
		claimed := map[string]bool{}
		// Like Disable, a run narrows only a namespace's own servers: a global
		// server keeps every tool its configuration allows.
		var selection agents.MCPToolSelection
		if config.Namespace != "" {
			selection = selected.Tools[name]
		}
		for _, tool := range listed {
			descriptor := tool.GetToolDescriptor()
			if descriptor == nil || descriptor.ToolUnion.OfFunction == nil {
				continue
			}
			original := descriptor.Name
			if original == "" {
				original = descriptor.ToolUnion.OfFunction.Name
			}
			if !selectedTool(selection, original) {
				continue
			}
			public := descriptor.ToolUnion.OfFunction.Name
			if exposed[public] || claimed[public] {
				conflict = public
				break
			}
			claimed[public] = true
			copy := *descriptor
			copy.Meta = maps.Clone(descriptor.Meta)
			copy.MCPServerName = name
			bound = append(bound, &boundTool{BaseTool: &copy, client: c})
		}

		// Tool names change upstream without a config edit, so a user connector that collides
		// is dropped on its own rather than failing the owner's runs. Globals keep failing
		// loudly: the developer owns both sides of the conflict.
		if conflict != "" {
			logUnavailable(ctx, namespace, name, fmt.Errorf("tool %q conflicts with another connector", conflict))
			if config.Namespace == "" {
				return nil, nil, fmt.Errorf("duplicate MCP tool %q; configure distinct tool prefixes", conflict)
			}
			statuses = append(statuses, agents.FailedConnectorStatus(name, fmt.Errorf("tool %q conflicts with another connector; configure a distinct tool prefix", conflict)))
			continue
		}
		for public := range claimed {
			exposed[public] = true
		}
		tools = append(tools, bound...)
		statuses = append(statuses, agents.ConnectedConnectorStatus(name, len(bound)))
		slog.InfoContext(ctx, "MCP server ready", slog.String("server", name), slog.String("namespace", namespace), slog.Int("tools", len(bound)))
	}
	return statuses, tools, nil
}

// logUnavailable logs a server a listing could not use, and why: its run goes
// on without its tools.
func logUnavailable(ctx context.Context, namespace, name string, err error) {
	slog.WarnContext(ctx, "MCP server unavailable", slog.String("server", name), slog.String("namespace", namespace), slog.Any("error", err))
}

func selectedTool(selection agents.MCPToolSelection, name string) bool {
	return (len(selection.Include) == 0 || slices.Contains(selection.Include, "*") || slices.Contains(selection.Include, name)) && !slices.Contains(selection.Exclude, "*") && !slices.Contains(selection.Exclude, name)
}

// CallTool re-resolves the namespace's definition at execution time without serializing secrets.
// Calls use the current store configuration, including edits made after discovery.
func (c *Client) CallTool(ctx context.Context, descriptor *agents.BaseTool, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
	if descriptor == nil || descriptor.MCPServerName == "" || call == nil {
		return nil, errors.New("MCP call requires a tool with a server name and a tool call")
	}

	// Resolve the current configuration for the authoritative execution namespace.
	resolved, err := c.listServerConfigs(ctx, call.Namespace, call.RunContext)
	if err != nil {
		return nil, err
	}
	config, exists := resolved.configs[descriptor.MCPServerName]
	if !exists {
		if err := resolved.invalid[descriptor.MCPServerName]; err != nil {
			return nil, fmt.Errorf("invalid MCP server configuration: %w", err)
		}
		return nil, errors.New("MCP server is no longer configured")
	}
	server, err := c.build(ctx, config)
	if err != nil {
		return nil, err
	}

	// Every tool executes through the same configured transport.
	return server.CallToolDirect(ctx, call.RunContext, descriptor, call)
}

type boundTool struct {
	*agents.BaseTool
	client *Client
}

func (t *boundTool) Execute(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
	return t.client.CallTool(ctx, t.BaseTool, call)
}

var _ agents.MCPClient = (*Client)(nil)
