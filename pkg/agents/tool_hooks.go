package agents

import (
	"context"
	"errors"
	"fmt"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

// ErrToolCallAborted is what a run fails with when a hook returned an error.
// Every hook error is wrapped in it on the way out of RunWithToolCallHooks, so
// a caller can tell a run a hook stopped from one a tool broke.
var ErrToolCallAborted = errors.New("tool call aborted by hook")

// abortedByHook marks a hook's error as the reason the run is ending.
//
// The mark records where the error came from, not what it means: an error out of
// a hook ends the run, an error out of the tool is reported to the model, and by
// the time both are ToolExecutionResult.Err the loop can no longer tell them
// apart. Only RunWithToolCallHooks is in a position to say, so only it marks —
// which is also why nothing has to carry the distinction across a durable
// boundary. A hook error is whatever the hook returned, wherever it ran.
func abortedByHook(err error) error {
	if err == nil {
		return ErrToolCallAborted
	}
	if IsToolCallAborted(err) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrToolCallAborted, err)
}

// IsToolCallAborted reports whether a run ended because a hook returned an
// error, as opposed to a tool failing or the caller going away. The hook's own
// error stays reachable with errors.Is and errors.As.
func IsToolCallAborted(err error) bool {
	return errors.Is(err, ErrToolCallAborted)
}

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
	// an error to end the run.
	//
	// An error is a hard stop, not a refusal: the chain stops where it is, the
	// tool never runs, and the run fails carrying that error. To refuse a call
	// and let the run carry on, answer it — HandleToolCall with the refusal as
	// the tool's output, which is what the model reads and works around:
	//
	//	return agents.HandleToolCall(agents.ToolCallResult(call, "not allowed for this user")), nil
	//
	// That is usually the kinder refusal, and it is a different decision from
	// failing, so it is said differently. Reserve the error for when continuing
	// would be worse than stopping.
	BeforeToolCall(ctx context.Context, serialisedTool *BaseTool, call *ToolCall) (ToolCallHookResult, error)

	// AfterToolCall runs once the call has a result — whichever produced it,
	// the tool or an earlier hook. Return ContinueToolCall to leave that result
	// alone, HandleToolCall to replace it, or an error to end the run, though
	// the tool has already run by then.
	//
	// It does not run on a paused call: a pause has no result yet, and the call
	// comes back through here when the run resumes.
	AfterToolCall(ctx context.Context, serialisedTool *BaseTool, call *ToolCall, result *ToolCallResponse) (ToolCallHookResult, error)
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
// Errors end the call rather than answering it, and which one failed decides
// what happens next. A hook's error ends the run, and is marked on the way out
// so the loop knows not to hand it to the model. exec's error is returned
// unmarked, leaving the loop free to report a broken tool to the model and let
// it try something else. Either way the after-hooks have nothing to observe.
func RunWithToolCallHooks(
	ctx context.Context,
	hooks []ToolCallHook,
	exec ExecutableToolCall,
	fn func(context.Context, *ToolCall) (*ToolCallResponse, error),
) (*ToolCallResponse, error) {
	var result *ToolCallResponse
	handled := false

	baseTool := serializeTool(exec)

	for _, hook := range hooks {
		if hook == nil {
			continue
		}

		res, err := hook.BeforeToolCall(ctx, baseTool, exec.ToolCall)
		if err != nil {
			return nil, abortedByHook(err)
		}
		if res.Handled {
			result, handled = answerFor(exec.ToolCall, res.Response), true
			break
		}
	}

	if !handled {
		var err error
		result, err = fn(ctx, exec.ToolCall)
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
		res, err := hook.AfterToolCall(ctx, baseTool, exec.ToolCall, result)
		if err != nil {
			return nil, abortedByHook(err)
		}
		if res.Handled {
			result = answerFor(exec.ToolCall, res.Response)
		}
	}

	return result, nil
}

// serializeTool is the tool as a hook is shown it: plain data, because the real tool
// may be a proxy for one running in another process, and there is nothing else
// a hook could portably be handed.
//
// A tool that cannot describe itself still gets its call checked — refusing to
// run hooks over it would fail open — so this always returns something. That
// something has to carry a ToolUnion: under a durable runtime it crosses to the
// hook's own step as an argument, and a BaseTool whose union is empty cannot be
// encoded at all (ToolUnion.MarshalJSON yields no bytes), which would fail the
// call rather than the identity. Naming it from the call is both encodable and
// more use to a hook than nothing.
func serializeTool(exec ExecutableToolCall) *BaseTool {
	if exec.Tool != nil {
		if encoded := exec.Tool.GetToolDescriptor(); encoded != nil && encoded.ToolUnion.OfFunction != nil {
			if encoded.Name != "" {
				return encoded
			}

			// Only a tool with a name of its own sets Name — an MCP server's
			// does, a function tool does not, because for it the model-facing
			// name is its name. Fill it in rather than making every hook know
			// that, on a copy: GetToolDescriptor may well have handed back the tool's
			// own embedded BaseTool, which is not ours to write to.
			named := *encoded
			named.Name = named.ToolUnion.OfFunction.Name
			return &named
		}
	}

	name := exec.ToolName
	if name == "" && exec.ToolCall != nil && exec.ToolCall.FunctionCallMessage != nil {
		name = exec.ToolCall.Name
	}

	return &BaseTool{
		Name:      name,
		ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{Name: name}},
	}
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
