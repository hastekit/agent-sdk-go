package restate_runtime

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	restate "github.com/restatedev/sdk-go"
)

type RestateLLM struct {
	restateCtx        restate.WorkflowContext
	wrappedLLM        llm.Provider
	providerConfigKey string

	// middlewares are the agent's real model-call middlewares, whose wraps run here,
	// inside the step.
	middlewares []agents.ModelCallMiddleware
}

func NewRestateLLM(restateCtx restate.WorkflowContext, wrappedLLM llm.Provider, providerConfigKey string, broker agents.StreamBroker, streamID string, middlewares ...agents.ModelCallMiddleware) agents.LLM {
	return &RestateLLM{
		restateCtx:        restateCtx,
		wrappedLLM:        wrappedLLM,
		providerConfigKey: providerConfigKey,
		middlewares:       append([]agents.ModelCallMiddleware{agents.StopMiddleware{Watcher: agents.StopWatcherFrom(broker), StreamID: streamID}}, middlewares...),
	}
}

func (l *RestateLLM) NewStreamingResponses(ctx context.Context, call *agents.ModelCall, in *responses.Request, cb func(chunk *responses.ResponseChunk)) (*responses.Response, error) {
	resp, err := restate.Run(l.restateCtx, func(ctx restate.RunContext) (*responses.Response, error) {
		return l.invoke(ctx, call, in, cb)
	}, restate.WithName("LLMCall"))

	// A call the stop cut short comes back as a terminal step failure. Report
	// it as the stop it is, so the loop ends the run cleanly instead of failing
	// it — the error code is all that survives the journal.
	if err != nil && wasCancelled(err) {
		return nil, agents.ErrModelCallStopped
	}
	return resp, err
}

// invoke is the body of the LLM step: where the model is really called, where
// the stop is watched, and where the middlewares' WrapModelCall runs — around the
// request as it arrived, so what the wraps hand the provider (the bytes behind
// an attachment file_id) reaches it and never the journal.
func (l *RestateLLM) invoke(ctx context.Context, call *agents.ModelCall, in *responses.Request, cb func(chunk *responses.ResponseChunk)) (*responses.Response, error) {
	// The Restate RunContext does not inherit the caller's context values,
	// so re-establish the provider config key that
	// gateway.ProviderConfigKeyFromContext (in the LLM client) reads.
	runCtx := gateway.WithProviderConfigKey(ctx, l.providerConfigKey)

	resp, err := agents.ExecuteModelCallWithMiddleware(runCtx, l.middlewares, call, in, func(ctx context.Context, _ *agents.ModelCall, in *responses.Request) (*responses.Response, error) {
		stream, err := l.wrappedLLM.NewStreamingResponses(ctx, in)
		if err != nil {
			return nil, err
		}

		acc := agents.Accumulator{}
		return acc.ReadStream(ctx, stream, func(chunk *responses.ResponseChunk) {
			cb(chunk)
		})
	})
	if err != nil {
		// Terminal, or Restate replays into this step and calls the model
		// again — the one thing a user who pressed stop must not get.
		return nil, cancellationError(err)
	}

	return resp, nil
}
