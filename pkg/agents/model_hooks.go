package agents

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// ModelCallHook wraps a call to the model: it sees the call on the way out and
// what it cost on the way back, and can answer for the model itself.
//
// The case it exists for is spending. A run calls the model once per loop
// iteration, and each call costs money against somebody's balance — so the
// question "may this run afford another call?" has to be asked before the call,
// and the answer recorded after it.
//
// Like ToolCallHook it is an interface rather than a pair of functions, so a
// durable runtime can substitute a proxy and turn each method into its own
// journaled step. A balance check that talks to a billing service must not be
// re-run every time a workflow replays.
type ModelCallHook interface {
	// GetName identifies the hook. A durable runtime uses it to name the
	// hook's steps, so it must be unique among the hooks on one agent and
	// stable across deploys.
	GetName() string

	// BeforeModelCall runs before the request goes to the provider. Return
	// ContinueModelCall to let it go, HandleModelCall to answer for the model,
	// or an error to fail the run.
	//
	// Handling is usually the kinder refusal: the response supplied becomes
	// this iteration's reply, so an out-of-credit run ends with an assistant
	// message the user can read rather than a failed run they cannot. Reserve
	// the error for when there is nothing sensible to say.
	BeforeModelCall(ctx context.Context, call *ModelCall) (ModelCallHookResult, error)

	// AfterModelCall runs once the provider has answered, with what the call
	// reported using. This is where spend is recorded.
	//
	// Return ContinueModelCall to leave the reply alone or HandleModelCall to
	// replace it. An error fails the run — the call has already been made and
	// paid for, so use it only when continuing would be worse.
	AfterModelCall(ctx context.Context, call *ModelCall, result *ModelCallResult) (ModelCallHookResult, error)
}

// ModelCall is one call to the model, as a hook sees it.
//
// It carries what the call is and who it belongs to, not the prompt itself: a
// budget check needs the model, the tenant, and how much of the window is
// already in use, and under a durable runtime everything here crosses to the
// hook's own step on every single iteration. Shipping the whole conversation
// there — twice, since it already crossed to the model's step — would cost more
// than the check it serves.
type ModelCall struct {
	AgentName  string         `json:"agent_name"`
	Namespace  string         `json:"namespace"`
	ThreadID   string         `json:"thread_id"`
	SessionID  string         `json:"session_id,omitempty"`
	StreamID   string         `json:"stream_id,omitempty"`
	RunID      string         `json:"run_id,omitempty"`
	RunContext map[string]any `json:"run_context,omitempty"`

	// Model is what the request will be sent to.
	Model string `json:"model"`

	// LoopIteration is the run's own loop counter, as MaxLoops measures it:
	// zero for the call that opens the run, and one more for each round of
	// tools since. Reported as the run tracks it rather than renumbered, so it
	// can be compared against a budget on loops directly.
	LoopIteration int `json:"loop_iteration"`

	// ContextTokens is how full the context window already is, by the same
	// reckoning the summarizer uses: the last measured prompt plus an estimate
	// of everything appended since. It is the best pre-call estimate of what
	// this call will cost on input.
	ContextTokens int `json:"context_tokens"`

	// Usage is what the run has spent so far, across every call it has made.
	Usage responses.Usage `json:"usage"`
}

// ModelCallResult is what the model's answer cost. It is a struct rather than
// a bare Usage so that what an after-hook can see may grow without every
// implementation having to change.
type ModelCallResult struct {
	// Usage is what the provider reported for this one call.
	Usage responses.Usage `json:"usage"`
}

// ModelCallHookResult is a hook's answer about one model call: whether it
// answered for the model, and with what.
//
// It is an explicit flag rather than a nil check on Response for the same
// reason ToolCallHookResult is: "I answered, and the answer is nothing to say"
// is a different thing from "carry on without me".
type ModelCallHookResult struct {
	// Handled says the hook answered for the model. Before the call that means
	// the provider is never contacted; after it, that Response replaces the
	// reply.
	Handled bool `json:"handled"`

	// Response is that answer. Build it with ModelCallText for the ordinary
	// case of an assistant message.
	Response *responses.Response `json:"response,omitempty"`
}

// ContinueModelCall lets the call go on: to the next hook, and then to the
// provider. It is the zero value, so a hook with nothing to say can also
// return ModelCallHookResult{}.
func ContinueModelCall() ModelCallHookResult {
	return ModelCallHookResult{}
}

// HandleModelCall answers for the model with resp, so the provider is not
// called (before) or its reply is replaced (after).
func HandleModelCall(resp *responses.Response) ModelCallHookResult {
	return ModelCallHookResult{Handled: true, Response: resp}
}

// ModelCallText builds the response a hook returns when it answers for the
// model: a single assistant message saying text, and no tool calls — so the
// loop takes it as the model's final word and the turn ends there.
//
// This is what an out-of-credit check returns. The user reads why the run
// stopped instead of seeing it fail.
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

// RunWithModelCallHooks runs a model call through its hooks: every
// BeforeModelCall in order until one answers, then call unless one did, then
// every AfterModelCall in order.
//
// Locally the hooks are the real ones and call reaches the provider; inside a
// workflow the hooks are proxies that journal each method as its own step —
// same order, same decisions, one call to the provider.
func RunWithModelCallHooks(
	ctx context.Context,
	hooks []ModelCallHook,
	call *ModelCall,
	invoke func(context.Context) (*responses.Response, error),
) (*responses.Response, error) {
	var response *responses.Response
	handled := false

	for _, hook := range hooks {
		if hook == nil {
			continue
		}
		res, err := hook.BeforeModelCall(ctx, call)
		if err != nil {
			return nil, err
		}
		if res.Handled {
			response, handled = res.Response, true
			break
		}
	}

	if !handled {
		var err error
		response, err = invoke(ctx)
		if err != nil {
			return nil, err
		}
	}

	result := &ModelCallResult{}
	if response != nil && response.Usage != nil {
		result.Usage = *response.Usage
	}

	for _, hook := range hooks {
		if hook == nil {
			continue
		}
		res, err := hook.AfterModelCall(ctx, call, result)
		if err != nil {
			return nil, err
		}
		if res.Handled {
			response = res.Response
		}
	}

	return response, nil
}
