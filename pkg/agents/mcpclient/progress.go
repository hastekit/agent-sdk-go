package mcpclient

import (
	"context"
	"maps"
	"sync"

	"github.com/google/uuid"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// progressReporters routes an in-flight MCP tool call's server-sent progress
// notifications back to the SDK-level ProgressReporter for that call. The MCP
// client is a package-level singleton shared across every session, so its
// ProgressNotificationHandler is global; we disambiguate by the progress
// token the tool set on the outgoing CallTool request. Entries are registered
// for the duration of a single CallTool and removed as soon as it returns.
var progressReporters sync.Map // map[string]agents.ProgressReporter

// registerProgress associates a progress token with a reporter and returns a
// cleanup func. token must be unique per in-flight call (the tool call id).
func registerProgress(token string, reporter agents.ProgressReporter) func() {
	if token == "" || reporter == nil {
		return func() {}
	}
	progressReporters.Store(token, reporter)
	return func() { progressReporters.Delete(token) }
}

// newCallToolParams builds CallTool params for a tool call, and when the call
// carries a ProgressReporter, attaches a progress token so the server streams
// notifications/progress and registers the reporter to receive them. The
// returned cleanup func unregisters the reporter and must be deferred by the
// caller. base (the tool's fixed Meta) is never mutated — a token is only ever
// set on a private copy.
func newCallToolParams(base mcp.Meta, name string, args map[string]any, tc *agents.ToolCall) (*mcp.CallToolParams, func()) {
	params := &mcp.CallToolParams{Meta: base, Name: name, Arguments: args}
	if tc == nil || tc.Progress == nil {
		return params, func() {}
	}
	token := tc.CallID
	if token == "" {
		token = uuid.NewString()
	}
	meta := mcp.Meta{}
	maps.Copy(meta, base)
	params.Meta = meta
	params.SetProgressToken(token)
	return params, registerProgress(token, tc.Progress)
}

// handleProgressNotification is the MCP client's ProgressNotificationHandler.
// It maps notifications/progress onto the SDK's ProgressReporter abstraction,
// mirroring how MCP elicitation is projected onto responses.Interrupt. It is
// best-effort: an unknown or missing token is silently dropped.
func handleProgressNotification(ctx context.Context, req *mcp.ProgressNotificationClientRequest) {
	if req == nil || req.Params == nil {
		return
	}
	token, ok := req.Params.ProgressToken.(string)
	if !ok || token == "" {
		return
	}
	v, ok := progressReporters.Load(token)
	if !ok {
		return
	}
	reporter, ok := v.(agents.ProgressReporter)
	if !ok || reporter == nil {
		return
	}
	reporter.Report(ctx, agents.ToolProgress{
		Progress: req.Params.Progress,
		Total:    req.Params.Total,
		Message:  req.Params.Message,
	})
}
