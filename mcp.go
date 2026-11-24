package sdk

import "github.com/hastekit/agent-sdk-go/pkg/agents/mcpclient"

// MCPStore lists MCP server definitions by namespace — see mcpclient.Store.
// An MCP client reads one as its built-in store and one as its user store.
type MCPStore = mcpclient.Store

// NewMCPClient reads global servers from a built-in store, such as an
// InlineStore, and each namespace's own from the store WithUserMCPStore gives.
var NewMCPClient = mcpclient.NewClient

// WithUserMCPStore reads each namespace's own MCP servers from a store.
var WithUserMCPStore = mcpclient.WithUserStore

// WithMCPCredentials supplies an MCP client's shared credential resolution.
var WithMCPCredentials = mcpclient.WithCredentials

// WithMCPSchemaCache supplies an MCP client's shared discovery caching.
var WithMCPSchemaCache = mcpclient.WithSchemaCache

// FilterMCPStore lists only the named servers of an MCP config store, each
// narrowed to the tools its ToolFilter allows.
var FilterMCPStore = mcpclient.Filter
