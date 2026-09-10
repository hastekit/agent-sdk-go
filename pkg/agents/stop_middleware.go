package agents

import (
	"context"
	"errors"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// StopMiddleware cancels the complete call chain when its stream is stopped.
// Executors install it outside user middleware, inside the execution boundary.
// Durable workflow proxies must not install it: the worker owns cancellation.
type StopMiddleware struct {
	NoopMiddleware

	Watcher StopWatcher
	// CancelGracePeriod applies to tools that ignore cancellation. Zero uses
	// DefaultCancelGrace. Model calls unwind cooperatively: abandoning them
	// could leave streaming callbacks writing after the durable step returns.
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
		if ctx.Err() == nil && callCtx.Err() != nil && errors.Is(err, context.Canceled) {
			return nil, ErrModelCallStopped
		}
		return response, err
	}
}
