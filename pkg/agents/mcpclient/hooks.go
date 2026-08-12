package mcpclient

import (
	"context"

	"github.com/hastekit/hastekit-sdk-go/pkg/agents"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/hastekit-sdk-go/pkg/utils"
)

// ToolCallHook runs immediately before an MCP tool call leaves for the server.
// It can let the call through, answer it itself, or refuse it.
//
// The call it receives is the whole request: the tool's name, the arguments
// the model chose, the thread and agent it belongs to, and RunContext — which
// is where a caller's own per-run values live (an inbound token, a tenant id,
// whatever Execute was handed). That is the pairing an access check needs: who
// is asking, and what they are asking for.
//
// The three outcomes:
//
//   - (nil, nil) — let the call proceed to the server. Later hooks still run.
//   - (resp, nil) — short-circuit: resp becomes the call's result and the
//     server is never contacted. Build it with ToolCallResult, or return a
//     response carrying Interrupts to pause the run instead of answering (an
//     unauthenticated caller can be sent to a login URL this way, and the run
//     resumes on the answer).
//   - (nil, err) — refuse: the error's text goes back to the model as the
//     call's result. This is shorthand for a short-circuit that says no.
//
// Neither refusing nor short-circuiting fails the run: an answer the model can
// read and work around is almost always more useful than a broken run.
//
// A hook may also amend the call — Arguments is writable — but it runs on the
// hot path of every call, so keep it quick and don't block on anything slow.
type ToolCallHook func(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error)

// ToolCallResult builds the response a hook returns when it answers a call
// itself. It stamps the call's ids onto the result, which is what pairs the
// answer with the function_call already in history.
func ToolCallResult(call *agents.ToolCall, output string) *agents.ToolCallResponse {
	return &agents.ToolCallResponse{
		FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			ID:     call.ID,
			CallID: call.CallID,
			Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr(output)},
		},
	}
}

// WithBeforeToolCall registers hooks that run before every tool call this
// client makes, in the order given. The first one to answer or refuse settles
// the call and the rest are skipped.
//
// Registering more than once appends rather than replaces, so independent
// concerns (an access check, an audit line) can be added separately without
// one quietly dropping the other.
func WithBeforeToolCall(hooks ...ToolCallHook) McpServerOption {
	return func(srv *MCPClient) {
		srv.beforeToolCall = append(srv.beforeToolCall, hooks...)
	}
}

// runBeforeToolCall applies the hooks in order. It returns a response when one
// of them answered the call, an error when one refused, and nil for both when
// the call should go to the server.
func runBeforeToolCall(ctx context.Context, hooks []ToolCallHook, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	for _, hook := range hooks {
		if hook == nil {
			continue
		}
		resp, err := hook(ctx, params)
		if err != nil {
			return nil, err
		}
		if resp != nil {
			return settleHookResponse(params, resp), nil
		}
	}
	return nil, nil
}

// settleHookResponse holds the invariant the whole loop rests on: every
// function_call in history is answered by exactly one output carrying its call
// id. A hand-built response can arrive without those ids, or without an output
// message at all, and an unanswered call would break the next request to the
// provider rather than the hook that caused it — so fill them in here.
//
// A response carrying interrupts is left alone: a pause has no result yet, by
// design, and the run answers the call when it resumes.
func settleHookResponse(params *agents.ToolCall, resp *agents.ToolCallResponse) *agents.ToolCallResponse {
	if len(resp.Interrupts) > 0 && resp.FunctionCallOutputMessage == nil {
		return resp
	}

	if resp.FunctionCallOutputMessage == nil {
		resp.FunctionCallOutputMessage = &responses.FunctionCallOutputMessage{
			Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr("")},
		}
	}
	if resp.ID == "" {
		resp.ID = params.ID
	}
	if resp.CallID == "" {
		resp.CallID = params.CallID
	}

	return resp
}
