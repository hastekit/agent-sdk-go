package agents

import (
	"context"

	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/hastekit-sdk-go/pkg/utils"
)

// ToolCallHook wraps a tool call: it sees the call on the way out and the
// result on the way back, and can settle either one itself.
//
// The call it receives is the whole request — the tool's name, the arguments
// the model chose, the thread and agent it belongs to, and RunContext, where a
// caller's own per-run values live (an inbound token, a tenant id, whatever
// Execute was handed). That is the pairing an access check needs: who is
// asking, and what they are asking for.
//
// It is an interface rather than a pair of functions so a durable runtime can
// substitute a proxy for it. Each method then becomes its own durable step —
// journaled once and never re-run on replay — which is what a hook that calls
// out to an authorization service needs, and what a plain function value could
// never be, since a function cannot cross a workflow boundary.
type ToolCallHook interface {
	// GetName identifies the hook. A durable runtime uses it to name the
	// hook's steps, so it must be unique among the hooks on one agent and
	// stable across deploys — renaming it orphans in-flight runs the same way
	// renaming an activity does.
	GetName() string

	// BeforeToolCall runs before the call leaves for the tool. Return
	// ContinueToolCall to let it through, HandleToolCall to answer it here, or
	// an error to refuse it — the error's text becomes the call's result.
	//
	// Neither refusing nor handling fails the run. An answer the model can read
	// and work around is almost always more useful than a broken run.
	BeforeToolCall(ctx context.Context, call *ToolCall) (ToolCallHookResult, error)

	// AfterToolCall runs once the call has a result — whichever produced it,
	// the tool or an earlier hook. Return ContinueToolCall to leave that result
	// alone, HandleToolCall to replace it, or an error to replace it with the
	// error's text.
	//
	// It does not run on a paused call: a pause has no result yet, and the call
	// comes back through here when the run resumes.
	AfterToolCall(ctx context.Context, call *ToolCall, result *ToolCallResponse) (ToolCallHookResult, error)
}

// ToolCallHookResult is a hook's answer about one call: whether it handled the
// call itself, and with what.
//
// It is an explicit flag rather than a nil check on Response because the two
// are different answers. "I handled this, and the answer is nothing to say" is
// not the same as "carry on without me", and a caller reading only the
// response cannot tell them apart.
type ToolCallHookResult struct {
	// Handled says the hook answered the call. Before the call that means the
	// tool never runs; after it, that Response replaces the result.
	Handled bool

	// Response is the answer. Build it with ToolCallResult so it carries the
	// call's ids — the loop pairs every result with its function_call by them.
	// A response carrying Interrupts pauses the run instead of answering it,
	// which is how an unauthenticated caller is sent to a login URL.
	Response *ToolCallResponse
}

// ContinueToolCall passes the call forward: to the next hook, and then to the
// tool. It is the zero value, so a hook with nothing to say can also return
// ToolCallHookResult{}.
func ContinueToolCall() ToolCallHookResult {
	return ToolCallHookResult{}
}

// HandleToolCall answers the call with resp, so the tool is not run (before) or
// its result is replaced (after).
func HandleToolCall(resp *ToolCallResponse) ToolCallHookResult {
	return ToolCallHookResult{Handled: true, Response: resp}
}

// ToolCallResult builds the response a hook returns when it answers a call
// itself. It stamps the call's ids onto the result, which is what pairs the
// answer with the function_call already in history.
func ToolCallResult(call *ToolCall, output string) *ToolCallResponse {
	return &ToolCallResponse{
		FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			ID:     call.ID,
			CallID: call.CallID,
			Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr(output)},
		},
	}
}

// RunWithToolCallHooks runs a tool call through its hooks: every
// BeforeToolCall in order until one settles the call, then exec unless it was
// settled, then every AfterToolCall in order.
//
// This is the seam a durable runtime replaces. Locally the hooks are the real
// ones and exec is the tool; inside a workflow the hooks are proxies that
// journal each method as its own step and exec is the tool's activity — same
// order, same decisions, one execution each.
//
// An error from exec is returned as-is rather than turned into a result: that
// is a run-level failure (a cancelled context, an activity that gave up), not
// an answer, and the after-hooks have nothing to observe.
func RunWithToolCallHooks(
	ctx context.Context,
	hooks []ToolCallHook,
	call *ToolCall,
	exec func(context.Context, *ToolCall) (*ToolCallResponse, error),
) (*ToolCallResponse, error) {
	var result *ToolCallResponse
	handled := false

	for _, hook := range hooks {
		if hook == nil {
			continue
		}
		res, err := hook.BeforeToolCall(ctx, call)
		if err != nil {
			result, handled = ToolCallResult(call, err.Error()), true
			break
		}
		if res.Handled {
			result, handled = answerFor(call, res.Response), true
			break
		}
	}

	if !handled {
		var err error
		result, err = exec(ctx, call)
		if err != nil {
			return nil, err
		}
	}

	// A pause is not a result. The call is answered when the run resumes and
	// comes back through here, which is also when the after-hooks should see
	// it — running them now would run them twice for one call.
	if result != nil && len(result.Interrupts) > 0 {
		return result, nil
	}

	for _, hook := range hooks {
		if hook == nil {
			continue
		}
		res, err := hook.AfterToolCall(ctx, call, result)
		if err != nil {
			result = ToolCallResult(call, err.Error())
			break
		}
		if res.Handled {
			result = answerFor(call, res.Response)
		}
	}

	return result, nil
}

// answerFor holds the invariant the whole loop rests on: every function_call in
// history is answered by exactly one output carrying its call id. A hook that
// says it handled a call has to leave one behind, and a hand-built response can
// arrive without the ids — an unanswered call would then break the next request
// to the provider rather than the hook that caused it.
//
// A response carrying interrupts is left alone: a pause has no result yet, by
// design.
func answerFor(call *ToolCall, resp *ToolCallResponse) *ToolCallResponse {
	if resp == nil {
		return ToolCallResult(call, "")
	}
	if len(resp.Interrupts) > 0 && resp.FunctionCallOutputMessage == nil {
		return resp
	}

	if resp.FunctionCallOutputMessage == nil {
		resp.FunctionCallOutputMessage = &responses.FunctionCallOutputMessage{
			Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr("")},
		}
	}
	if resp.ID == "" {
		resp.ID = call.ID
	}
	if resp.CallID == "" {
		resp.CallID = call.CallID
	}

	return resp
}
