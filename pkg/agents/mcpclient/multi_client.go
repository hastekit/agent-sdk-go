package mcpclient

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
)

// Client manages store-defined MCP servers for multiple namespaces.
// Global definitions win over namespace definitions; duplicate global names are errors.
// Invalid, duplicate, or colliding user-owned definitions fail only their own connector.
type Client struct {
	stores      []MCPServerConfigStore
	credentials CredentialProvider
	cache       SchemaCache
}

// NewClient reads all supplied stores at discovery and execution time.
// Configure credentials and schema caching before sharing the client across agents.
func NewClient(stores ...MCPServerConfigStore) *Client {
	return &Client{stores: slices.Clone(stores)}
}

// WithCredentials supplies shared credential resolution; OAuth settings live in ServerConfig.
func (c *Client) WithCredentials(provider CredentialProvider) *Client {
	c.credentials = provider
	return c
}

// WithSchemaCache supplies optional shared discovery caching.
func (c *Client) WithSchemaCache(cache SchemaCache) *Client {
	c.cache = cache
	return c
}

// catalog is one resolved view of the definitions visible to a namespace.
type catalog struct {
	// configs holds usable definitions by name; a global shadows a same-named user definition.
	configs map[string]ServerConfig
	// invalid holds user-owned definitions that cannot be used, by name. They are reported as
	// failed connectors rather than failing the owner's whole catalog, and remain deletable.
	invalid map[string]error
}

// listServerConfigs resolves one visible catalog before doing network I/O, enforcing scope and precedence.
// Store failures, cross-namespace records, and invalid globals are errors: they are application faults.
// Invalid or duplicate user-owned records are isolated so one bad record cannot lock its owner out.
func (c *Client) listServerConfigs(ctx context.Context, namespace string, runContext map[string]any) (catalog, error) {
	// Read every source before merging ownership scopes.
	if err := ctx.Err(); err != nil {
		return catalog{}, err
	}
	var configs []ServerConfig
	for _, store := range c.stores {
		if store == nil {
			return catalog{}, errors.New("MCP config store must not be nil")
		}
		listed, err := store.ListServerConfigs(ctx, namespace, runContext)
		if err != nil {
			return catalog{}, fmt.Errorf("list MCP server configurations: %w", err)
		}
		configs = append(configs, listed...)
	}

	// Classify records by owner; globals are validated strictly, user records individually.
	globals := map[string]ServerConfig{}
	users := map[string]ServerConfig{}
	invalid := map[string]error{}
	for _, config := range configs {
		if config.Namespace != "" && config.Namespace != namespace {
			return catalog{}, errors.New("MCP store returned a server from another namespace")
		}
		if config.Namespace == "" {
			if err := validateServerConfig(config); err != nil {
				return catalog{}, err
			}
			if _, exists := globals[config.Name]; exists {
				return catalog{}, fmt.Errorf("duplicate global MCP server %q", config.Name)
			}
			copy, err := cloneServerConfig(config)
			if err != nil {
				return catalog{}, err
			}
			globals[config.Name] = copy
			continue
		}

		// A name recorded twice in one scope is ambiguous, whichever copy is valid.
		if _, exists := users[config.Name]; exists || invalid[config.Name] != nil {
			delete(users, config.Name)
			invalid[config.Name] = fmt.Errorf("duplicate MCP server %q in one scope", config.Name)
			continue
		}
		if err := validateServerConfig(config); err != nil {
			invalid[config.Name] = err
			continue
		}
		copy, err := cloneServerConfig(config)
		if err != nil {
			invalid[config.Name] = err
			continue
		}
		users[config.Name] = copy
	}

	// Globals win independently of store ordering, including over invalid user records.
	result := catalog{configs: globals, invalid: map[string]error{}}
	for name, config := range users {
		if _, exists := globals[name]; !exists {
			result.configs[name] = config
		}
	}
	for name, err := range invalid {
		if _, exists := globals[name]; !exists {
			result.invalid[name] = err
		}
	}
	return result, nil
}

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

	// Only definitions that request credentials use the shared provider.
	if config.UseCredentials || config.Authorization != nil {
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
			statuses = append(statuses, agents.FailedConnectorStatus(name, fmt.Errorf("invalid MCP server configuration: %w", resolved.invalid[name])))
			continue
		}

		// A global misconfiguration fails the run; a user's own only fails that connector.
		server, err := c.build(ctx, config)
		if err != nil {
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
			statuses = append(statuses, agents.FailedConnectorStatus(name, err))
			continue
		}

		// Narrow configured tools and retain the server name for execution.
		var bound []agents.Tool
		var conflict string
		claimed := map[string]bool{}
		selection := selected.Tools[name]
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
	}
	return statuses, tools, nil
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
