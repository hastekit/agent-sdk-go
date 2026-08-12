package mcpclient

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/hastekit/hastekit-sdk-go/pkg/agents"
	"github.com/hastekit/hastekit-sdk-go/pkg/utils"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type MCPClient struct {
	Endpoint  string            `json:"-"`
	Transport string            `json:"-"`
	Headers   map[string]string `json:"-"`

	Session               *mcp.ClientSession `json:"-"`
	Tools                 []*mcp.Tool        `json:"-"`
	Meta                  mcp.Meta           `json:"-"`
	ToolFilter            []string           `json:"-"`
	ApprovalRequiredTools []string           `json:"-"`
	DeferredTools         []string           `json:"-"`
	CacheTTL              time.Duration      `json:"-"`
	DisableStandaloneSSE  bool               `json:"-"`
	schemaCache           SchemaCache        // injected cache (required for caching)

	// beforeToolCall gates every call this client makes — see
	// WithBeforeToolCall.
	beforeToolCall []ToolCallHook
}

func NewClient(ctx context.Context, endpoint string, options ...McpServerOption) (*MCPClient, error) {
	srv := &MCPClient{
		Endpoint: endpoint,
	}

	for _, option := range options {
		option(srv)
	}

	return srv, nil
}

type McpServerOption func(*MCPClient)

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

func (srv *MCPClient) GetName() string {
	return "MCPClient"
}

func (srv *MCPClient) GetClient(ctx context.Context, runContext map[string]any) (*MCPClient, error) {
	// resolve the headers with run context
	headers := map[string]string{}
	for k, v := range srv.Headers {
		headers[k] = utils.TryAndParseAsTemplate(v, runContext)
	}

	session, err := connect(ctx, srv.Endpoint, srv.Transport, headers, srv.DisableStandaloneSSE)
	if err != nil {
		return nil, err
	}

	tools, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		return nil, err
	}

	return &MCPClient{
		Endpoint:              srv.Endpoint,
		Headers:               headers,
		Session:               session,
		Tools:                 tools.Tools,
		Meta:                  srv.Meta,
		ToolFilter:            srv.ToolFilter,
		ApprovalRequiredTools: srv.ApprovalRequiredTools,
		DeferredTools:         srv.DeferredTools,
		DisableStandaloneSSE:  srv.DisableStandaloneSSE,
		beforeToolCall:        srv.beforeToolCall,
	}, nil
}

func (srv *MCPClient) GetTools(opts ...McpServerOption) []agents.Tool {
	mcpTools := []agents.Tool{}

	for _, o := range opts {
		o(srv)
	}

	for _, tool := range srv.Tools {
		// Filter tools
		if len(srv.ToolFilter) > 0 && !slices.Contains(srv.ToolFilter, tool.Name) {
			continue
		}

		// Check if tool requires approval
		requiresApproval := false
		if len(srv.ApprovalRequiredTools) > 0 && slices.Contains(srv.ApprovalRequiredTools, tool.Name) {
			requiresApproval = true
		}

		// Check if tool is deferred
		deferred := false
		if len(srv.DeferredTools) > 0 && slices.Contains(srv.DeferredTools, tool.Name) {
			deferred = true
		}

		mcpTools = append(mcpTools, NewMcpTool(tool, srv.Session, srv.Meta, requiresApproval, deferred, srv.beforeToolCall...))
	}

	return mcpTools
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

// CallToolDirect calls an MCP tool by name without listing tools first.
// Uses the connection pool for efficient connection reuse.
func (srv *MCPClient) CallToolDirect(ctx context.Context, runContext map[string]any, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	resolvedHeaders := srv.resolveHeaders(runContext)

	// This is the path a durable runtime takes — the workflow holds only a
	// serialized tool definition and calls back here to execute — so the hooks
	// have to be attached here too, or the gate would apply on the local
	// runtime and silently not on Temporal or Restate.
	//
	// The run context comes in as its own argument on this path. Put it on the
	// call so a hook reads it in the one place it reads it everywhere else.
	if params != nil && params.RunContext == nil {
		params.RunContext = runContext
	}

	tool := &LazyMcpTool{
		endpoint:             srv.Endpoint,
		transportType:        srv.Transport,
		resolvedHeaders:      resolvedHeaders,
		meta:                 srv.Meta,
		toolName:             params.Name,
		disableStandaloneSSE: srv.DisableStandaloneSSE,
		beforeToolCall:       srv.beforeToolCall,
	}
	return tool.Execute(ctx, params)
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

// buildLazyTools converts cached mcp.Tool schemas into LazyMcpTool instances,
// applying tool filters, approval flags, and deferred flags.
func (srv *MCPClient) buildLazyTools(tools []*mcp.Tool, meta mcp.Meta, resolvedHeaders map[string]string) []agents.Tool {
	var result []agents.Tool
	for _, tool := range tools {
		if len(srv.ToolFilter) > 0 && !slices.Contains(srv.ToolFilter, tool.Name) {
			continue
		}

		requiresApproval := len(srv.ApprovalRequiredTools) > 0 && slices.Contains(srv.ApprovalRequiredTools, tool.Name)
		deferred := len(srv.DeferredTools) > 0 && (slices.Contains(srv.DeferredTools, tool.Name) || slices.Contains(srv.DeferredTools, "*"))

		result = append(result, NewLazyMcpTool(tool, srv.Endpoint, srv.Transport, resolvedHeaders, meta, srv.DisableStandaloneSSE, requiresApproval, deferred, srv.beforeToolCall...))
	}
	return result
}
