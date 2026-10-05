package agents

import (
	"context"
	"slices"
)

// MCPSelection selects servers and their original, unprefixed tool names for a run.
// Global servers are always enabled. Namespace servers are enabled unless disabled.
// Unknown names are ignored across handoffs. It mirrors SkillSelection.
// Selection cannot expose tools excluded by the server's configured ToolFilter.
type MCPSelection struct {
	Disable []string                    `json:"disable,omitempty"`
	Tools   map[string]MCPToolSelection `json:"tools,omitempty"`
}

// MCPToolSelection narrows a server's tools. An empty Include means all configured tools.
type MCPToolSelection struct {
	Include []string `json:"include,omitempty"`
	Exclude []string `json:"exclude,omitempty"`
}

// MCPClient owns discovery, selection, connector status, and execution across MCP servers.
// Listing failures for individual servers are statuses; store/configuration failures are errors.
// Implementations must support concurrent namespaces without mutating shared run configuration.
type MCPClient interface {
	ListTools(context.Context, string, map[string]any, ...MCPSelection) ([]ConnectorStatus, []Tool, error)
	CallTool(context.Context, *BaseTool, *ToolCall) (*ToolCallResponse, error)
}

// MCPListing crosses durable boundaries using only statuses and tool descriptors.
type MCPListing struct {
	Connectors []ConnectorStatus `json:"connectors"`
	Tools      []BaseTool        `json:"tools"`
}

// Clone isolates mutable selection when a continuation outlives the current run.
func (s MCPSelection) Clone() MCPSelection {
	copy := MCPSelection{Disable: slices.Clone(s.Disable)}
	if s.Tools != nil {
		copy.Tools = make(map[string]MCPToolSelection, len(s.Tools))
		for name, selection := range s.Tools {
			copy.Tools[name] = MCPToolSelection{Include: slices.Clone(selection.Include), Exclude: slices.Clone(selection.Exclude)}
		}
	}
	return copy
}
