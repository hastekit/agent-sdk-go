package mcpclient

import (
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Resolve into new containers so cached tools can execute concurrently without
// retaining another run's values. Template semantics match WithHeaders.
func resolveMeta(base mcp.Meta, runContext map[string]any) mcp.Meta {
	if base == nil {
		return nil
	}
	out := make(mcp.Meta, len(base))
	for key, value := range base {
		out[key] = resolveMetaValue(value, runContext)
	}
	return out
}

func resolveMetaValue(value any, runContext map[string]any) any {
	switch value := value.(type) {
	case string:
		return utils.TryAndParseAsTemplate(value, runContext)
	case map[string]any:
		return map[string]any(resolveMeta(mcp.Meta(value), runContext))
	case mcp.Meta:
		return resolveMeta(value, runContext)
	case map[string]string:
		return resolveTemplates(value, runContext)
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = resolveMetaValue(item, runContext)
		}
		return out
	case []string:
		out := make([]string, len(value))
		for i, item := range value {
			out[i] = utils.TryAndParseAsTemplate(item, runContext)
		}
		return out
	default:
		return value
	}
}
