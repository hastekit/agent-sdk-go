package mcpclient

import (
	"context"
	"errors"

	"github.com/bytedance/sonic"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/hastekit-sdk-go/pkg/utils"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type McpTool struct {
	*agents.BaseTool
	Session *mcp.ClientSession `json:"-"`
	Meta    mcp.Meta           `json:"-"`
}

func NewMcpTool(t *mcp.Tool, session *mcp.ClientSession, Meta mcp.Meta, requiresApproval bool, deferred bool) *McpTool {
	inputSchema := map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
	inputSchemaBytes, err := sonic.Marshal(t.InputSchema)
	if err == nil {
		_ = sonic.Unmarshal(inputSchemaBytes, &inputSchema)
	}

	return &McpTool{
		BaseTool: &agents.BaseTool{
			RequiresApproval: requiresApproval,
			Deferred:         deferred,
			ToolUnion: responses.ToolUnion{
				OfFunction: &responses.FunctionTool{
					Name:        t.Name,
					Description: utils.Ptr(t.Description),
					Parameters:  inputSchema,
					Strict:      utils.Ptr(false),
				},
			},
		},
		Session: session,
		Meta:    Meta,
	}
}

func (c *McpTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	var args map[string]any
	if params.Arguments != "" {
		err := sonic.Unmarshal([]byte(params.Arguments), &args)
		if err != nil {
			return &agents.ToolCallResponse{
				FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
					ID:     params.ID,
					CallID: params.CallID,
					Output: responses.FunctionCallOutputContentUnion{
						OfString: utils.Ptr(err.Error()),
					},
				},
			}, nil
		}
	}

	// Call the MCP tool. When the run wired a progress sink, attach a
	// progress token so the server streams notifications/progress back
	// through handleProgressNotification for the duration of the call.
	callParams, cleanup := newCallToolParams(c.Meta, params.Name, args, params)
	defer cleanup()

	res, err := c.Session.CallTool(ctx, callParams)
	if err != nil {
		// A cancelled call is not a failed tool: report it as an error so
		// the loop records it as cancelled, rather than handing the model
		// "context canceled" as the tool's answer.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}

		return &agents.ToolCallResponse{
			FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
				ID:     params.ID,
				CallID: params.CallID,
				Output: responses.FunctionCallOutputContentUnion{
					OfString: utils.Ptr(err.Error()),
				},
			},
		}, nil
	}

	// Return the tool result
	for _, r := range res.Content {
		if tc, ok := r.(*mcp.TextContent); ok {
			out := &responses.FunctionCallOutputMessage{
				ID:     params.ID,
				CallID: params.CallID,
				Output: responses.FunctionCallOutputContentUnion{
					OfString: utils.Ptr(tc.Text),
				},
			}
			return &agents.ToolCallResponse{FunctionCallOutputMessage: out}, nil
		}
	}

	err = errors.New("missing mcp tool result")
	return nil, err
}

// LazyMcpTool holds a cached tool schema but defers MCP connection to Execute() time.
// This allows ListTools() to return tool definitions without establishing a live connection.
type LazyMcpTool struct {
	*agents.BaseTool
	endpoint             string
	transportType        string
	resolvedHeaders      map[string]string
	meta                 mcp.Meta
	toolName             string
	disableStandaloneSSE bool
}

func NewLazyMcpTool(t *mcp.Tool, endpoint, transportType string, resolvedHeaders map[string]string, meta mcp.Meta, disableStandaloneSSE bool, requiresApproval bool, deferred bool) *LazyMcpTool {
	inputSchema := map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
	inputSchemaBytes, err := sonic.Marshal(t.InputSchema)
	if err == nil {
		_ = sonic.Unmarshal(inputSchemaBytes, &inputSchema)
	}

	return &LazyMcpTool{
		BaseTool: &agents.BaseTool{
			RequiresApproval: requiresApproval,
			Deferred:         deferred,
			ToolUnion: responses.ToolUnion{
				OfFunction: &responses.FunctionTool{
					Name:        t.Name,
					Description: utils.Ptr(t.Description),
					Parameters:  inputSchema,
					Strict:      utils.Ptr(false),
				},
			},
		},
		endpoint:             endpoint,
		transportType:        transportType,
		resolvedHeaders:      resolvedHeaders,
		meta:                 meta,
		toolName:             t.Name,
		disableStandaloneSSE: disableStandaloneSSE,
	}
}

func (c *LazyMcpTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	var args map[string]any
	if params.Arguments != "" {
		err := sonic.Unmarshal([]byte(params.Arguments), &args)
		if err != nil {
			return &agents.ToolCallResponse{
				FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
					ID:     params.ID,
					CallID: params.CallID,
					Output: responses.FunctionCallOutputContentUnion{
						OfString: utils.Ptr(err.Error()),
					},
				},
			}, nil
		}
	}

	// Get a connection from the pool (or create a new one)
	cli, err := globalPool.Checkout(ctx, c.endpoint, c.transportType, c.resolvedHeaders, c.disableStandaloneSSE)
	if err != nil {
		return &agents.ToolCallResponse{
			FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
				ID:     params.ID,
				CallID: params.CallID,
				Output: responses.FunctionCallOutputContentUnion{
					OfString: utils.Ptr(err.Error()),
				},
			},
		}, nil
	}

	// Call the MCP tool directly by name — no ListTools needed. When the run
	// wired a progress sink, attach a progress token so the server streams
	// notifications/progress back through handleProgressNotification.
	callParams, cleanup := newCallToolParams(c.meta, params.Name, args, params)
	defer cleanup()

	res, err := cli.CallTool(ctx, callParams)
	if err != nil {
		// The caller gave up — not a broken connection. Treating it as one
		// would drop the session that carries the notifications/cancelled
		// for this request, and the retry below would start the tool again
		// on the server.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}

		// Connection might be dead — remove from pool and retry once
		globalPool.Remove(c.endpoint, c.transportType, c.resolvedHeaders)
		cli, retryErr := globalPool.Checkout(ctx, c.endpoint, c.transportType, c.resolvedHeaders, c.disableStandaloneSSE)
		if retryErr != nil {
			return &agents.ToolCallResponse{
				FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
					ID:     params.ID,
					CallID: params.CallID,
					Output: responses.FunctionCallOutputContentUnion{
						OfString: utils.Ptr(err.Error()),
					},
				},
			}, nil
		}
		res, err = cli.CallTool(ctx, callParams)
		if err != nil {
			return &agents.ToolCallResponse{
				FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
					ID:     params.ID,
					CallID: params.CallID,
					Output: responses.FunctionCallOutputContentUnion{
						OfString: utils.Ptr(err.Error()),
					},
				},
			}, nil
		}
	}

	// Return the tool result
	for _, r := range res.Content {
		if tc, ok := r.(*mcp.TextContent); ok {
			out := &responses.FunctionCallOutputMessage{
				ID:     params.ID,
				CallID: params.CallID,
				Output: responses.FunctionCallOutputContentUnion{
					OfString: utils.Ptr(tc.Text),
				},
			}
			return &agents.ToolCallResponse{FunctionCallOutputMessage: out}, nil
		}
	}

	err = errors.New("missing mcp tool result")
	return nil, err
}
