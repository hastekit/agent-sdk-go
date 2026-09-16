package agents

import (
	"context"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// StopMiddleware cancels the call chain when its stream is stopped.
// Executors install it inside the execution boundary, outside user middleware.
// Durable proxies do not install it; the worker owns cancellation.
type StopMiddleware struct {
	NoopMiddleware

	Watcher StopWatcher
	// CancelGracePeriod applies to tools that ignore cancellation. Zero uses
	// DefaultCancelGrace. Model calls must unwind cooperatively because their
	// streaming callbacks may still be active.
	CancelGracePeriod time.Duration
	// StreamID overrides call metadata when the runtime owns the stream ID.
	// Otherwise the middleware uses ToolCall.StreamID or ModelCall.StreamID.
	StreamID string
}

var _ Middleware = StopMiddleware{}

func (m StopMiddleware) WrapToolCall(next ToolCallFunc) ToolCallFunc {
	return func(ctx context.Context, tool *BaseTool, call *ToolCall) (*ToolCallResponse, error) {
		streamID := m.StreamID
		if streamID == "" && call != nil {
			streamID = call.StreamID
		}
		return RunStoppable(ctx, m.Watcher, m.CancelGracePeriod, streamID, func(ctx context.Context) (*ToolCallResponse, error) {
			return next(ctx, tool, call)
		})
	}
}

func (m StopMiddleware) WrapModelCall(next ModelCallFunc) ModelCallFunc {
	return func(ctx context.Context, call *ModelCall, request *responses.Request) (*responses.Response, error) {
		streamID := m.StreamID
		if streamID == "" && call != nil {
			streamID = call.StreamID
		}
		callCtx, cancel := StopCancelContext(ctx, m.Watcher, streamID)
		defer cancel()
		response, err := next(callCtx, call, request)
		// Once stop cancellation is active, any failed call is a stopped call.
		// Providers do not all return context.Canceled consistently. A call that
		// completed before the stop keeps its response.
		if err != nil && ctx.Err() == nil && callCtx.Err() != nil {
			return nil, ErrModelCallStopped
		}
		return response, err
	}
}
