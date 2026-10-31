package agents

import (
	"context"
	"maps"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// ToolCallCreatedAtMetadataKey is the ToolCallResponse.Metadata entry holding
// when the result was produced. Read it with ToolCallResponse.CreatedAt.
const ToolCallCreatedAtMetadataKey = responses.CreatedAtMetadataKey

// TimestampMiddleware records when each model reply and tool result was
// produced (Response.Metadata and ToolCallResponse.Metadata, under
// responses.CreatedAtMetadataKey). The loop keeps it as the stored message's
// CreatedAt.
//
// It is built in: installed beside StopMiddleware at every execution boundary,
// so it runs inside the model and tool steps of a durable runtime and its
// reading is journaled with the result rather than taken again on replay.
type TimestampMiddleware struct{}

var (
	_ ModelCallMiddleware = TimestampMiddleware{}
	_ ToolCallMiddleware  = TimestampMiddleware{}
)

// WrapModelCall stamps the reply with when it came back.
func (TimestampMiddleware) WrapModelCall(next ModelCallFunc) ModelCallFunc {
	return func(ctx context.Context, call *ModelCall, request *responses.Request) (*responses.Response, error) {
		response, err := next(ctx, call, request)
		if err != nil || response == nil {
			return response, err
		}
		stamped := *response
		stamped.Metadata = maps.Clone(response.Metadata)
		stamped.SetCreatedAt(time.Now())
		return &stamped, nil
	}
}

// WrapToolCall stamps the result with when it came back.
func (TimestampMiddleware) WrapToolCall(next ToolCallFunc) ToolCallFunc {
	return func(ctx context.Context, tool *BaseTool, call *ToolCall) (*ToolCallResponse, error) {
		response, err := next(ctx, tool, call)
		if response == nil {
			return response, err
		}
		stamped := *response
		stamped.Metadata = maps.Clone(response.Metadata)
		stamped.SetCreatedAt(time.Now())
		return &stamped, err
	}
}

// SetCreatedAt records when the result was produced in Metadata.
func (r *ToolCallResponse) SetCreatedAt(at time.Time) {
	if r.Metadata == nil {
		r.Metadata = map[string]any{}
	}
	r.Metadata[ToolCallCreatedAtMetadataKey] = at
}

// CreatedAt reads what SetCreatedAt recorded; ok is false when it was not
// recorded. It accepts the JSON form that arrives after the response crossed
// a durable runtime's boundary.
func (r *ToolCallResponse) CreatedAt() (at time.Time, ok bool, err error) {
	if r == nil {
		return at, false, nil
	}
	return responses.CreatedAtFromMetadata(r.Metadata)
}
