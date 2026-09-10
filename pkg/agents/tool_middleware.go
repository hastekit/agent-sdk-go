package agents

import (
	"context"
	"errors"
	"fmt"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

// ErrToolCallAborted is what a run fails with when a middleware returned an error of
// its own. Every such error is wrapped in it on the way out, so a caller can
// tell a run a middleware stopped from one a tool broke.
var ErrToolCallAborted = errors.New("tool call aborted by middleware")

// abortedByMiddleware marks a middleware's error as the reason the run is ending. The mark
// records where the error came from, not what it means: an error out of a middleware
// ends the run, an error out of the tool is reported to the model, and by the
// time both are ToolExecutionResult.Err the loop can no longer tell them apart.
func abortedByMiddleware(err error) error {
	if err == nil {
		return ErrToolCallAborted
	}
	if IsToolCallAborted(err) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrToolCallAborted, err)
}

// IsToolCallAborted reports whether a run ended because a middleware returned an
// error, as opposed to a tool failing or the caller going away. The middleware's own
// error stays reachable with errors.Is and errors.As.
func IsToolCallAborted(err error) bool {
	return errors.Is(err, ErrToolCallAborted)
}

// ToolCallMiddleware wraps every tool call an agent makes, middleware-style.
//
// It is an interface rather than a function so a durable runtime can hold the
// real middleware on the worker and run it inside the activity or step that runs
// the tool.
type ToolCallMiddleware interface {
	// WrapToolCall returns the function that runs in place of next. Call next
	// to run the tool and return what should stand as the result, or answer
	// the call without calling next at all. The call carries the tool's name,
	// the arguments the model chose, the run it belongs to, and RunContext,
	// where a caller's own per-run values live; tool is the tool itself as
	// plain data, since the real one may be a proxy for one running elsewhere.
	//
	// A refusal is an answer, not a failure: return ToolCallResult with the
	// reason and the model reads it and works around it. An error of the
	// middleware's own ends the run. An error from next is the tool's and is
	// reported to the model as one, so pass it back as it came. Return results
	// as given or as copies; never write to the one supplied.
	//
	// It runs inside the execution boundary, before the result is journaled:
	// the tool activity or run step under a durable runtime, the executor's
	// own goroutine locally. It is not a separate durable step, and it may run
	// again if that step retries. Middlewares nest in registration order, the first
	// outermost: a result flows from the tool through the last middleware's wrap to
	// the first's.
	//
	// A background tool has two execution boundaries: Execute starts the task,
	// and AwaitTask waits for completion. Each runs its own chain. The wait
	// carries the original call ID and the task stream ID; short-circuiting it
	// skips waiting, but does not cancel the already-started external task.
	WrapToolCall(next ToolCallFunc) ToolCallFunc
}

// ToolCallFunc is a tool call as a middleware wraps it: given the tool and the call,
// it produces the result. It is what WrapToolCall is handed as next, and what
// it returns.
type ToolCallFunc func(ctx context.Context, tool *BaseTool, call *ToolCall) (*ToolCallResponse, error)

// ToolCallResult builds the response a middleware returns when it answers a call
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

// ExecuteToolWithMiddleware runs one tool call through its middlewares' WrapToolCall
// chain, first middleware outermost, and pairs the result with the call.
//
// This is the seam a durable runtime replaces. Locally the middlewares are the real
// ones and fn is the tool; inside a workflow the executor holds no middlewares and
// fn is the tool's activity, which runs the real chain on the worker.
//
// A middleware's error ends the run and comes back marked (see IsToolCallAborted),
// so the loop knows not to hand it to the model; a tool's error comes back as
// it was, leaving the loop free to report a broken tool to the model.
func ExecuteToolWithMiddleware(
	ctx context.Context,
	middlewares []ToolCallMiddleware,
	exec ExecutableToolCall,
	fn func(context.Context, *ToolCall) (*ToolCallResponse, error),
) (*ToolCallResponse, error) {
	result, err := ExecuteToolCallWithMiddleware(ctx, middlewares, serializeTool(exec), exec.ToolCall, fn)
	if err != nil {
		return nil, err
	}
	return withCallIDs(exec.ToolCall, result), nil
}

// serializeTool is the tool as a middleware is shown it: plain data, because the real
// tool may be a proxy for one running in another process, and there is nothing
// else a middleware could portably be handed.
//
// A tool that cannot describe itself still gets its call checked — refusing to
// run middlewares over it would fail open — so this always returns something. That
// something has to carry a ToolUnion: under a durable runtime it crosses to the
// worker as an argument, and a BaseTool whose union is empty cannot be encoded
// at all (ToolUnion.MarshalJSON yields no bytes), which would fail the call
// rather than the identity. Naming it from the call is both encodable and more
// use to a middleware than nothing.
func serializeTool(exec ExecutableToolCall) *BaseTool {
	if exec.Tool != nil {
		if encoded := exec.Tool.GetToolDescriptor(); encoded != nil && encoded.ToolUnion.OfFunction != nil {
			if encoded.Name != "" {
				return encoded
			}

			// Only a tool with a name of its own sets Name — an MCP server's
			// does, a function tool does not, because for it the model-facing
			// name is its name. Fill it in rather than making every middleware know
			// that, on a copy: GetToolDescriptor may well have handed back the
			// tool's own embedded BaseTool, which is not ours to write to.
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

// withCallIDs holds the invariant the whole loop rests on: every function_call
// in history is answered by exactly one output carrying its call id. A middleware
// that answers a call by hand can leave the ids off, and an unanswered call
// would then break the next request to the provider rather than the middleware that
// caused it. A response with no output — a pause, a background task — is left
// alone.
func withCallIDs(call *ToolCall, resp *ToolCallResponse) *ToolCallResponse {
	if call == nil || resp == nil || resp.FunctionCallOutputMessage == nil {
		return resp
	}
	if resp.ID == "" {
		resp.ID = call.ID
	}
	if resp.CallID == "" {
		resp.CallID = call.CallID
	}
	return resp
}

// WrapToolCall builds the chain of every middleware's WrapToolCall around next, the
// first middleware outermost: the first registered is the last to see a result on
// its way back. Nil middlewares are skipped.
func WrapToolCall(middlewares []ToolCallMiddleware, next ToolCallFunc) ToolCallFunc {
	for i := len(middlewares) - 1; i >= 0; i-- {
		if middlewares[i] == nil {
			continue
		}
		next = middlewares[i].WrapToolCall(next)
	}
	return next
}

// toolFailure marks an error as the tool's own while it travels back out
// through the middlewares' wraps. A wrap returns whatever it likes, and by the time
// an error leaves the chain nothing else says whether the tool failed — which
// the model should hear about — or a middleware did — which ends the run. Wrapping
// on the way in and looking for the mark on the way out settles it without
// asking middlewares to mark anything themselves: they only have to pass the tool's
// error back, which errors.As sees through however they wrap it.
type toolFailure struct{ err error }

func (f *toolFailure) Error() string { return f.err.Error() }
func (f *toolFailure) Unwrap() error { return f.err }

// ExecuteToolCallWithMiddleware runs fn through every middleware's WrapToolCall, first middleware
// outermost, inside the caller's execution boundary — which is where durable
// runtimes call it, so what the wraps make of the result is what the step
// returns and journals. A middleware's error comes back marked as the run's end (see
// IsToolCallAborted); the tool's own error comes back as it was.
func ExecuteToolCallWithMiddleware(ctx context.Context, middlewares []ToolCallMiddleware, tool *BaseTool, call *ToolCall, fn func(context.Context, *ToolCall) (*ToolCallResponse, error)) (*ToolCallResponse, error) {
	inner := func(ctx context.Context, _ *BaseTool, call *ToolCall) (*ToolCallResponse, error) {
		result, err := fn(ctx, call)
		if err != nil {
			return nil, &toolFailure{err}
		}
		return result, nil
	}
	result, err := WrapToolCall(middlewares, inner)(ctx, tool, call)
	if err != nil {
		var failure *toolFailure
		if errors.As(err, &failure) {
			if err == failure {
				// Nothing added to it on the way out; hand back the original.
				return nil, failure.err
			}
			// A middleware said something about it. Keep that, and the identity
			// underneath for errors.Is.
			return nil, err
		}
		// Cancellation middleware is control flow, not a policy refusal.
		if errors.Is(err, ErrToolCancelled) || (ctx.Err() != nil && errors.Is(err, ctx.Err())) {
			return nil, err
		}
		return nil, abortedByMiddleware(err)
	}
	return result, nil
}
