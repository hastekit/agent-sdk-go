package sdk

import "github.com/hastekit/agent-sdk-go/pkg/agents/mcpclient"

var NewMCPClient = mcpclient.NewClient

// FilterMCPStore lists only the named servers of an MCP config store, global
// or user-owned, each narrowed to the tools its ToolFilter allows.
var FilterMCPStore = mcpclient.Filter
