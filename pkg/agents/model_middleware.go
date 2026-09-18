package agents

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// ModelCallMiddleware wraps every model call made by an agent. It can inspect
// or change the request, short-circuit the call, or inspect the response.
//
// Durable runtimes run middleware inside the same activity or step as the
// provider call.
type ModelCallMiddleware interface {
	// WrapModelCall returns the function that runs in place of next, with the
	// call the request belongs to. Call next to reach the provider: what next
	// receives is what the provider is sent, and what the wrap returns is the
	// reply. To answer in the model's place, return a reply without calling
	// next; ModelCallText builds the usual one. To put a note in front of the
	// model for this call only, hand next a copy of the request with the note
	// appended: the loop keeps its own request, so the note is never stored.
	//
	// Return your own error for a middleware failure. Return next's error when
	// the provider failed, so retry policies can classify it correctly.
	//
	// It runs inside the provider step and may run again if that step retries.
	// Do not mutate the supplied request. Middlewares are nested in registration
	// order; the first registered middleware is outermost.
	WrapModelCall(next ModelCallFunc) ModelCallFunc
}

// ModelCallFunc sends a request to the provider, on behalf of call, and
// returns the reply. It is the shape of next in WrapModelCall and of the
// function a wrap returns.
type ModelCallFunc func(ctx context.Context, call *ModelCall, request *responses.Request) (*responses.Response, error)

// ModelCall describes the call a wrap is around: which agent, thread and run
// it belongs to, the model, and what the run has used so far.
type ModelCall struct {
	AgentName  string         `json:"agent_name"`
	Namespace  string         `json:"namespace"`
	ThreadID   string         `json:"thread_id"`
	SessionID  string         `json:"session_id,omitempty"`
	StreamID   string         `json:"stream_id,omitempty"`
	RunID      string         `json:"run_id,omitempty"`
	RunContext map[string]any `json:"run_context,omitempty"`

	// Model is the model the request is addressed to.
	Model string `json:"model"`

	// Target explicitly overrides routing for this attempt (Provider/model).
	Target string `json:"target,omitempty"`

	// LoopIteration is the run's loop counter as MaxLoops counts it: zero
	// for the first call of the run, one more after each round of tools.
	LoopIteration int `json:"loop_iteration"`

	// ContextTokens estimates how full the context window is: the last
	// measured prompt plus an estimate of everything appended since.
	ContextTokens int `json:"context_tokens"`

	// Usage is what the run has consumed so far, across all its calls.
	Usage responses.Usage `json:"usage"`

	// State is a snapshot of the run's key-value scratchpad, the same one
	// tools read through ToolCall.State and write through
	// ToolCallResponse.StateUpdates. A wrap reads it; writes to the copy
	// reach nothing.
	State map[string]string `json:"state,omitempty"`
}

// ModelCallText builds a reply of a single assistant message and no tool
// calls, so the loop takes it as the model's final word and the turn ends. It
// is the usual answer of a wrap that refuses a call, such as one over budget.
func ModelCallText(text string) *responses.Response {
	return &responses.Response{
		Output: []responses.OutputMessageUnion{{
			OfOutputMessage: &responses.OutputMessage{
				ID:   responses.NewOutputItemMessageID(),
				Role: constants.RoleAssistant,
				Content: &responses.OutputContent{
					{OfOutputText: &responses.OutputTextContent{Text: text}},
				},
			},
		}},
	}
}

// WrapModelCall chains the middleware around next. The first middleware is
// outermost; nil entries are skipped.
func WrapModelCall(middlewares []ModelCallMiddleware, next ModelCallFunc) ModelCallFunc {
	for i := len(middlewares) - 1; i >= 0; i-- {
		if middlewares[i] == nil {
			continue
		}
		next = guardModelPublication(middlewares[i].WrapModelCall(next))
	}
	return next
}

// ExecuteModelCallWithMiddleware runs invoke through the middleware chain.
// Durable runtimes call it inside the provider step, so request changes stay
// transient and are never journaled. A nil call is replaced with an empty one.
// Middleware errors are marked as terminal; provider errors retain their
// original classification. A nil response without an error is invalid.
func ExecuteModelCallWithMiddleware(ctx context.Context, middlewares []ModelCallMiddleware, call *ModelCall, request *responses.Request, invoke ModelCallFunc) (*responses.Response, error) {
	if call == nil {
		call = &ModelCall{}
	}
	ctx = context.WithValue(ctx, modelPublicationKey{}, &atomic.Bool{})
	inner := func(ctx context.Context, call *ModelCall, request *responses.Request) (*responses.Response, error) {
		response, err := invoke(ctx, call, request)
		if err != nil {
			if IsModelStreamTransformError(err) {
				return nil, err
			}
			return nil, &providerFailure{err}
		}
		return response, nil
	}
	response, err := guardModelPublication(WrapModelCall(middlewares, inner))(ctx, call, request)
	if err != nil {
		return nil, modelCallOutcome(ctx, err)
	}
	if response == nil {
		return nil, abortedModelCall(fmt.Errorf("model call returned no response"))
	}
	return response, nil
}
