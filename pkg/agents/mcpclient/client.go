package mcpclient

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type MCPClient struct {
	Name      string            `json:"-"`
	Endpoint  string            `json:"-"`
	Transport string            `json:"-"`
	Headers   map[string]string `json:"-"`

	Session               *mcp.ClientSession `json:"-"`
	Tools                 []*mcp.Tool        `json:"-"`
	Meta                  mcp.Meta           `json:"-"`
	ToolFilter            []string           `json:"-"`
	ApprovalRequiredTools []string           `json:"-"`
	DeferredTools         []string           `json:"-"`
	ToolPrefix            string             `json:"-"`
	CacheTTL              time.Duration      `json:"-"`
	DisableStandaloneSSE  bool               `json:"-"`
	schemaCache           SchemaCache        // injected cache (required for caching)
}

func NewClient(ctx context.Context, endpoint string, options ...McpServerOption) (*MCPClient, error) {
	srv := &MCPClient{
		Endpoint: endpoint,
	}

	for _, option := range options {
		option(srv)
	}

	if srv.Name == "" {
		srv.Name = srv.Endpoint
	}

	// Copied, not written into: WithMeta stores the caller's own map, and two
	// clients built from one map would otherwise overwrite each other's
	// server_name — and the caller's map besides.
	meta := map[string]any{}
	maps.Copy(meta, srv.Meta)
	meta["server_name"] = srv.Name
	srv.Meta = meta

	return srv, nil
}

type McpServerOption func(*MCPClient)

func WithName(name string) McpServerOption {
	return func(server *MCPClient) {
		server.Name = name
	}
}

func WithHeaders(headers map[string]string) McpServerOption {
	return func(server *MCPClient) {
		server.Headers = headers
	}
}

func WithToolFilter(toolFilter ...string) McpServerOption {
	return func(srv *MCPClient) {
		srv.ToolFilter = toolFilter
	}
}

// WithToolPrefix namespaces this server's tools in the name the model sees,
// which keeps two servers that both publish a "search" from colliding.
//
// The prefix is used verbatim, separator included: WithToolPrefix("xyz__")
// exposes the server's "search" as "xyz__search". Passing "xyz" would produce
// "xyzsearch", so carry the separator in the prefix.
//
// The prefix is presentation only. Calls are made on the server under its own
// name, and WithToolFilter, WithApprovalRequiredTools and WithDeferredTools are
// all written against the server's own tool names, so they keep working
// unchanged when a prefix is added.
func WithToolPrefix(prefix string) McpServerOption {
	return func(srv *MCPClient) {
		srv.ToolPrefix = prefix
	}
}

func WithApprovalRequiredTools(tools ...string) McpServerOption {
	return func(srv *MCPClient) {
		srv.ApprovalRequiredTools = tools
	}
}

func WithDeferredTools(tools ...string) McpServerOption {
	return func(srv *MCPClient) {
		srv.DeferredTools = tools
	}
}

func WithTransport(transport string) McpServerOption {
	return func(srv *MCPClient) {
		if transport == "" {
			srv.Transport = "sse"
		} else {
			srv.Transport = transport
		}
	}
}

func WithCacheTTL(ttl time.Duration) McpServerOption {
	return func(srv *MCPClient) {
		srv.CacheTTL = ttl
	}
}

// WithDisableStandaloneSSE disables the post-init server→client SSE stream
// on the streamable-http transport. Enable it for servers that don't
// support the standalone GET stream (the client otherwise hangs waiting
// on a stream that never opens). Has no effect on the sse transport.
func WithDisableStandaloneSSE(disable bool) McpServerOption {
	return func(srv *MCPClient) {
		srv.DisableStandaloneSSE = disable
	}
}

// WithSchemaCache injects a SchemaCache implementation for caching tool schemas.
// When set, ListTools() will check the cache before connecting to the MCP server.
// This enables multi-pod cache sharing when backed by Redis or similar stores.
func WithSchemaCache(cache SchemaCache) McpServerOption {
	return func(srv *MCPClient) {
		srv.schemaCache = cache
	}
}

func WithMeta(m map[string]any) McpServerOption {
	return func(srv *MCPClient) {
		srv.Meta = m
	}
}

func (srv *MCPClient) GetName() string {
	return "MCPClient"
}

func (srv *MCPClient) ListTools(ctx context.Context, runContext map[string]any) ([]agents.Tool, error) {
	resolvedHeaders := srv.resolveHeaders(runContext)

	// If a schema cache is configured, check it first
	if srv.schemaCache != nil {
		key := srv.schemaCacheKey(resolvedHeaders)

		if cached, ok := srv.schemaCache.Get(ctx, key); ok {
			return srv.buildLazyTools(cached.Tools, cached.Meta, resolvedHeaders), nil
		}

		// Cache miss: connect, fetch schemas, cache, then disconnect
		tools, meta, err := srv.fetchToolSchemas(ctx, resolvedHeaders)
		if err != nil {
			return nil, err
		}

		srv.schemaCache.Set(ctx, key, &CachedToolEntry{Tools: tools, Meta: meta})
		return srv.buildLazyTools(tools, meta, resolvedHeaders), nil
	}

	// No cache configured: connect, fetch schemas, return lazy tools (no caching)
	tools, meta, err := srv.fetchToolSchemas(ctx, resolvedHeaders)
	if err != nil {
		return nil, err
	}

	return srv.buildLazyTools(tools, meta, resolvedHeaders), nil
}

// CallToolDirect runs one tool against this server without listing tools first,
// reusing a pooled connection. Listing instead would cost a fresh MCP session
// and a tools/list before every single call, since schemas are only cached when
// a SchemaCache is injected.
//
// It takes the tool rather than looking one up by name. A durable runtime's
// workflow already holds the tool as serialized data and hands it back here, so
// there is nothing to resolve: tool.Name is the name the server knows, already
// free of any prefix the model-facing name carries. That also means a tool the
// filter excluded cannot be called here — one that was never listed has no
// BaseTool to pass.
func (srv *MCPClient) CallToolDirect(ctx context.Context, runContext map[string]any, tool *agents.BaseTool, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	if tool == nil || tool.Name == "" {
		return nil, fmt.Errorf("mcp: cannot call %q without the tool it names", params.Name)
	}

	resolvedHeaders := srv.resolveHeaders(runContext)

	// The run context comes in as its own argument on this path — a durable
	// runtime's workflow holds only a serialized tool definition and calls back
	// here to execute. Put it on the call so anything downstream reads it in
	// the one place it reads it everywhere else.
	if params != nil && params.RunContext == nil {
		params.RunContext = runContext
	}

	lazy := &LazyMcpTool{
		BaseTool:             tool,
		endpoint:             srv.Endpoint,
		transportType:        srv.Transport,
		resolvedHeaders:      resolvedHeaders,
		meta:                 srv.Meta,
		disableStandaloneSSE: srv.DisableStandaloneSSE,
	}
	return lazy.Execute(ctx, params)
}

// InvalidateToolCache removes cached tool schemas for this MCP server.
func (srv *MCPClient) InvalidateToolCache(ctx context.Context, runContext map[string]any) {
	if srv.schemaCache == nil {
		return
	}
	resolvedHeaders := srv.resolveHeaders(runContext)
	key := srv.schemaCacheKey(resolvedHeaders)
	srv.schemaCache.Delete(ctx, key)
}

// InvalidateAllToolCache removes all cached tool schemas from the injected cache.
func (srv *MCPClient) InvalidateAllToolCache(ctx context.Context) {
	if srv.schemaCache == nil {
		return
	}
	srv.schemaCache.Clear(ctx)
}

// resolveHeaders resolves template variables in headers using the runContext.
func (srv *MCPClient) resolveHeaders(runContext map[string]any) map[string]string {
	headers := make(map[string]string, len(srv.Headers))
	for k, v := range srv.Headers {
		headers[k] = utils.TryAndParseAsTemplate(v, runContext)
	}
	return headers
}

// schemaCacheKey generates a cache key for tool schemas.
func (srv *MCPClient) schemaCacheKey(resolvedHeaders map[string]string) string {
	filterStr := ""
	if len(srv.ToolFilter) > 0 {
		sorted := make([]string, len(srv.ToolFilter))
		copy(sorted, srv.ToolFilter)
		for i := 0; i < len(sorted); i++ {
			for j := i + 1; j < len(sorted); j++ {
				if sorted[i] > sorted[j] {
					sorted[i], sorted[j] = sorted[j], sorted[i]
				}
			}
		}
		for _, f := range sorted {
			filterStr += f + ","
		}
	}

	return fmt.Sprintf("mcp:schema:%s|%s|%s|%s", srv.Endpoint, srv.Transport, sortedHeadersString(resolvedHeaders), filterStr)
}

// fetchToolSchemas connects to the MCP server, fetches tool schemas, and closes the connection.
func (srv *MCPClient) fetchToolSchemas(ctx context.Context, resolvedHeaders map[string]string) ([]*mcp.Tool, mcp.Meta, error) {
	session, err := connect(ctx, srv.Endpoint, srv.Transport, resolvedHeaders, srv.DisableStandaloneSSE)
	if err != nil {
		return nil, nil, err
	}

	tools, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		session.Close()
		return nil, nil, err
	}

	// Close the connection — we only needed the schemas.
	// Actual tool execution will use the connection pool.
	session.Close()

	return tools.Tools, srv.Meta, nil
}

// buildLazyTools converts mcp.Tool schemas into LazyMcpTool instances, applying
// tool filters, approval flags, and deferred flags. The schemas arrive already
// carrying the prefix (see fetchToolSchemas), so name is the model-facing name
// throughout.
func (srv *MCPClient) buildLazyTools(tools []*mcp.Tool, meta mcp.Meta, resolvedHeaders map[string]string) []agents.Tool {
	var result []agents.Tool
	for _, tool := range tools {
		if len(srv.ToolFilter) > 0 && !slices.Contains(srv.ToolFilter, tool.Name) {
			continue
		}

		requiresApproval := slices.Contains(srv.ApprovalRequiredTools, tool.Name)
		deferred := slices.Contains(srv.DeferredTools, tool.Name) || slices.Contains(srv.DeferredTools, "*")

		result = append(result, NewLazyMcpTool(tool, srv.Endpoint, srv.Transport, resolvedHeaders, meta, srv.DisableStandaloneSSE, requiresApproval, deferred, srv.ToolPrefix))
	}
	return result
}
