package mcpclient

import (
	"context"
	"log/slog"
	"maps"
	"strings"
	"time"
	"unicode/utf8"

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
	callParams, cleanup := newCallToolParams(resolveMeta(c.Meta, params.RunContext), params.Name, args, params)
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

	return mcpToolResult(params, res.Content)
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

// Execute calls the tool on its server, and logs the call: the server, the
// tool, the headers it went with, credentials redacted, and how it went.
func (c *LazyMcpTool) Execute(ctx context.Context, params *agents.ToolCall) (_ *agents.ToolCallResponse, err error) {
	call := &toolCallLog{start: time.Now()}
	defer func() { c.logCall(ctx, params, call, err) }()

	var args map[string]any
	if params.Arguments != "" {
		if err := sonic.Unmarshal([]byte(params.Arguments), &args); err != nil {
			call.failure = err
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

	meta := resolveMeta(c.meta, params.RunContext)

	cli, release, checkoutErr := checkoutSession(ctx, c.conn)
	if checkoutErr != nil {
		call.failure = checkoutErr
		return toolOutput(params, checkoutErr.Error()), nil
	}
	defer release()

	// Call the MCP tool directly by name — no ListTools needed. When the run
	// wired a progress sink, attach a progress token so the server streams
	// notifications/progress back through handleProgressNotification.
	callParams, cleanup := newCallToolParams(meta, c.Name, args, params)
	defer cleanup()

	// When resuming, carry the user's answer to the question the server asked
	// before the run paused.
	resumeElicitation(callParams, params)

	res, callErr := cli.CallTool(ctx, callParams)
	if callErr != nil {
		// The caller gave up — not a broken connection. Treating it as one
		// would drop the session that carries the notifications/cancelled
		// for this request, and the retry below would start the tool again
		// on the server.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}

		// A connection this call opened for itself cannot be a stale one the
		// pool held on to, so there is nothing a second attempt would fix.
		if !c.conn.poolable() {
			call.failure = callErr
			return toolOutput(params, callErr.Error()), nil
		}

		// Connection might be dead — remove from pool and retry once
		globalPool.Remove(c.conn)
		retried, retryRelease, retryErr := globalPool.Checkout(ctx, c.conn)
		if retryErr != nil {
			call.failure = callErr
			return toolOutput(params, callErr.Error()), nil
		}
		defer retryRelease()

		call.retried = true
		res, callErr = retried.CallTool(ctx, callParams)
		if callErr != nil {
			call.failure = callErr
			return toolOutput(params, callErr.Error()), nil
		}
	}
	call.result = res

	// The server wants something from the user rather than having an answer:
	// pause the run on it.
	pause, pauseErr := elicitationPause(params, res)
	if pauseErr != nil {
		call.failure = pauseErr
		return toolOutput(params, pauseErr.Error()), nil
	}
	if pause != nil {
		call.paused = true
		return pause, nil
	}

	return mcpToolResult(params, res.Content)
}

// toolCallLog is what a tool call's log says of how it went.
type toolCallLog struct {
	start time.Time
	// failure is what the model was shown in place of a result.
	failure error
	// result is the server's answer.
	result *mcp.CallToolResult
	// retried is a call made again on a new connection.
	retried bool
	// paused is a call the server asked the user something on.
	paused bool
}

// maxLoggedToolError bounds how much of a tool's error a log keeps.
const maxLoggedToolError = 500

// logCall logs a tool call: its server, where that is and the headers the call
// went with, credentials redacted, the tool, the run's namespace, and how it
// went, a tool's error with as much of its text as maxLoggedToolError allows.
func (c *LazyMcpTool) logCall(ctx context.Context, params *agents.ToolCall, call *toolCallLog, err error) {
	attrs := append(c.conn.logAttrs(),
		slog.String("tool", c.Name),
		slog.String("namespace", params.Namespace),
		slog.String("call_id", params.CallID),
		slog.Bool("retried", call.retried),
		slog.Duration("duration", time.Since(call.start)),
	)
	switch {
	case err != nil:
		slog.WarnContext(ctx, "MCP tool call failed", append(attrs, slog.Any("error", err))...)
	case call.failure != nil:
		slog.WarnContext(ctx, "MCP tool call failed", append(attrs, slog.Any("error", call.failure))...)
	case call.paused:
		slog.InfoContext(ctx, "MCP tool call paused for the user's answer", attrs...)
	case call.result != nil && call.result.IsError:
		slog.WarnContext(ctx, "MCP tool call returned an error", append(attrs, slog.String("result", toolErrorText(call.result)))...)
	default:
		slog.InfoContext(ctx, "MCP tool call", attrs...)
	}
}

// toolErrorText is the text of a tool's error result, up to maxLoggedToolError
// bytes, cut at a character's boundary.
func toolErrorText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, content := range res.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			b.WriteString(text.Text)
		}
		if b.Len() > maxLoggedToolError {
			break
		}
	}
	text := b.String()
	if len(text) <= maxLoggedToolError {
		return text
	}
	cut := maxLoggedToolError
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "…"
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

// checkoutSession returns the session this call runs on, and what to do with it
// when the call is done.
//
// Either way the release must be called when the caller is done. A pooled
// session outlives the call, so releasing it only says the call is over —
// which is what keeps the pool from sweeping a connection that is still
// carrying a request. An unpooled one belongs to this call alone and is closed
// with it; see serverConn.poolable for which is which, and why a connection
// carrying credentials nobody identified is never shared.
func checkoutSession(ctx context.Context, conn serverConn) (*mcp.ClientSession, func(), error) {
	if !conn.poolable() {
		session, err := connect(ctx, conn)
		if err != nil {
			return nil, nil, err
		}
		return session, func() { session.Close() }, nil
	}

	return globalPool.Checkout(ctx, conn)
}
