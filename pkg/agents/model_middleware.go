package agents

import (
	"context"
	"fmt"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// ModelCallMiddleware wraps every call an agent makes to the model, middleware-style.
// Its typical job is spend control: check a balance before calling next,
// record what the reply used after, and answer in the model's place when the
// balance is gone.
//
// It is an interface rather than a function so a durable runtime can hold the
// real middleware on the worker and run it inside the activity or step that calls
// the provider.
type ModelCallMiddleware interface {
	// WrapModelCall returns the function that runs in place of next, with the
	// call the request belongs to. Call next to reach the provider: what next
	// receives is what the provider is sent, and what the wrap returns is the
	// reply. To answer in the model's place, return a reply without calling
	// next; ModelCallText builds the usual one. To put a note in front of the
	// model for this call only, hand next a copy of the request with the note
	// appended: the loop keeps its own request, so the note is never stored.
	// An error fails the run.
	//
	// It runs inside the step that calls the provider (the LLM activity under
	// Temporal, the run step under Restate, the loop itself locally), never as
	// a step of its own, and may run again if that step retries. Never write
	// to the request supplied. Middlewares nest in registration order, the first
	// outermost: the first registered sees the request first and the reply
	// last.
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

// WrapModelCall chains every middleware's WrapModelCall around next, first middleware
// outermost. Nil middlewares are skipped.
func WrapModelCall(middlewares []ModelCallMiddleware, next ModelCallFunc) ModelCallFunc {
	for i := len(middlewares) - 1; i >= 0; i-- {
		if middlewares[i] == nil {
			continue
		}
		next = middlewares[i].WrapModelCall(next)
	}
	return next
}

// ExecuteModelCallWithMiddleware runs invoke through the middlewares' wraps. The loop calls
// it with the real middlewares locally; durable runtimes call it on the real middlewares
// inside the step that calls the provider, so whatever a wrap adds to the
// request is sent from there and never journaled. A nil call is shown to the
// wraps as an empty one. A reply that is nil without an error is a wrap's
// mistake, and is reported here rather than dereferenced later.
func ExecuteModelCallWithMiddleware(ctx context.Context, middlewares []ModelCallMiddleware, call *ModelCall, request *responses.Request, invoke ModelCallFunc) (*responses.Response, error) {
	if call == nil {
		call = &ModelCall{}
	}
	response, err := WrapModelCall(middlewares, invoke)(ctx, call, request)
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, fmt.Errorf("model call returned no response")
	}
	return response, nil
}
