package mcpclient

import (
	"context"
	"errors"
	"maps"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// PrefixedToolName is the name a tool is exposed to the model under. The prefix
// is used verbatim — a caller wanting "xyz__search" passes "xyz__" — and an
// empty one leaves the name exactly as the server gave it.
func PrefixedToolName(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + name
}

type McpTool struct {
	*agents.BaseTool
	Session *mcp.ClientSession `json:"-"`
	Meta    mcp.Meta           `json:"-"`
}

// toolAnnotations converts a server's tool annotations into the SDK's own
// shape, so MCP tools and locally defined function tools present identical
// hints to a permission policy.
//
// The MCP wire types carry readOnlyHint and idempotentHint as bare bools, so
// an absent hint is already false by the time we see it — which is the
// spec's default for both, and is what we forward. destructiveHint and
// openWorldHint arrive as pointers and stay unset when the server said
// nothing, leaving the conservative defaults (destructive, open world) to the
// Is* helpers.
func toolAnnotations(t *mcp.Tool) *agents.ToolAnnotations {
	if t == nil || t.Annotations == nil {
		return nil
	}

	a := t.Annotations
	return &agents.ToolAnnotations{
		Title:           a.Title,
		ReadOnlyHint:    utils.Ptr(a.ReadOnlyHint),
		DestructiveHint: a.DestructiveHint,
		IdempotentHint:  utils.Ptr(a.IdempotentHint),
		OpenWorldHint:   a.OpenWorldHint,
	}
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
			Annotations:      toolAnnotations(t),
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

	// When resuming, carry the user's answer to the question the server asked
	// before the run paused.
	resumeElicitation(callParams, params)

	res, err := c.Session.CallTool(ctx, callParams)
	if err != nil {
		// A cancelled call is not a failed tool: report it as an error so
		// the loop records it as cancelled, rather than handing the model
		// "context canceled" as the tool's answer.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}

		return toolOutput(params, err.Error()), nil
	}

	// The server wants something from the user rather than having an answer:
	// pause the run on it.
	pause, err := elicitationPause(params, res)
	if err != nil {
		return toolOutput(params, err.Error()), nil
	}
	if pause != nil {
		return pause, nil
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

	// conn says which server to reach and how, and is what the pool is keyed
	// by. meta is the client's own, sent with every call.
	conn serverConn
	meta mcp.Meta
}

func NewLazyMcpTool(t *mcp.Tool, conn serverConn, meta mcp.Meta, requiresApproval bool, deferred bool, toolPrefix string) *LazyMcpTool {
	inputSchema := map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
	inputSchemaBytes, err := sonic.Marshal(t.InputSchema)
	if err == nil {
		_ = sonic.Unmarshal(inputSchemaBytes, &inputSchema)
	}

	mergedMeta := map[string]any{}
	maps.Copy(mergedMeta, t.GetMeta())
	maps.Copy(mergedMeta, meta)

	return &LazyMcpTool{
		BaseTool: &agents.BaseTool{
			// The server's own name. Every call this tool makes is made under
			// it, while the model sees the prefixed one below — so the prefix
			// never has to be undone at call time.
			Name:             t.Name,
			RequiresApproval: requiresApproval,
			Deferred:         deferred,
			Annotations:      toolAnnotations(t),
			Meta:             mergedMeta,
			ToolUnion: responses.ToolUnion{
				OfFunction: &responses.FunctionTool{
					Name:        PrefixedToolName(toolPrefix, t.Name),
					Description: utils.Ptr(t.Description),
					Parameters:  inputSchema,
					Strict:      utils.Ptr(false),
				},
			},
		},
		conn: conn,
		meta: meta,
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
	cli, err := globalPool.Checkout(ctx, c.conn)
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
	callParams, cleanup := newCallToolParams(c.meta, c.Name, args, params)
	defer cleanup()

	// When resuming, carry the user's answer to the question the server asked
	// before the run paused.
	resumeElicitation(callParams, params)

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
		globalPool.Remove(c.conn)
		cli, retryErr := globalPool.Checkout(ctx, c.conn)
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

	// The server wants something from the user rather than having an answer:
	// pause the run on it.
	pause, err := elicitationPause(params, res)
	if err != nil {
		return toolOutput(params, err.Error()), nil
	}
	if pause != nil {
		return pause, nil
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

// toolOutput wraps text as this call's result, which is how a failure the
// model should see and work around is reported (as opposed to a Go error,
// which fails the run).
func toolOutput(params *agents.ToolCall, text string) *agents.ToolCallResponse {
	return &agents.ToolCallResponse{
		FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			ID:     params.ID,
			CallID: params.CallID,
			Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr(text)},
		},
	}
}
