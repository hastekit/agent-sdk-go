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

	// broker is how the step learns the run was stopped — the raw broker, not
	// the handler-side proxy, since the watch runs inside the step.
	broker agents.StreamBroker

	// streamID is the channel the run is stopped on.
	streamID string
}

func NewRestateLLM(restateCtx restate.WorkflowContext, wrappedLLM llm.Provider, providerConfigKey string, broker agents.StreamBroker, streamID string) agents.LLM {
	return &RestateLLM{
		restateCtx:        restateCtx,
		wrappedLLM:        wrappedLLM,
		providerConfigKey: providerConfigKey,
		broker:            broker,
		streamID:          streamID,
	}
}

func (l *RestateLLM) NewStreamingResponses(ctx context.Context, in *responses.Request, cb func(chunk *responses.ResponseChunk)) (*responses.Response, error) {
	resp, err := restate.Run(l.restateCtx, func(ctx restate.RunContext) (*responses.Response, error) {
		// The Restate RunContext is minted fresh and does not inherit the
		// caller's context values, so re-establish the provider config key
		// that gateway.ProviderConfigKeyFromContext (in the LLM client) reads.
		runCtx := gateway.WithProviderConfigKey(ctx, l.providerConfigKey)

		// The stop is watched from inside the step, where the provider's
		// request is: abandoning the step from the handler side would end the
		// wait and leave the model streaming.
		runCtx, cancel := agents.StopCancelContext(runCtx, agents.StopWatcherFrom(l.broker), l.streamID)
		defer cancel()

		stream, err := l.wrappedLLM.NewStreamingResponses(runCtx, in)
		if err != nil {
			return nil, cancellationError(err)
		}

		acc := agents.Accumulator{}
		resp, err := acc.ReadStream(runCtx, stream, func(chunk *responses.ResponseChunk) {
			cb(chunk)
		})
		if err != nil {
			// Terminal, or Restate replays into this step and calls the model
			// again — the one thing a user who pressed stop must not get.
			return nil, cancellationError(err)
		}

		return resp, nil
	}, restate.WithName("LLMCall"))

	// A call the stop cut short comes back as a terminal step failure. Report
	// it as the stop it is, so the loop ends the run cleanly instead of failing
	// it — the error code is all that survives the journal.
	if err != nil && wasCancelled(err) {
		return nil, agents.ErrModelCallStopped
	}
	return resp, err
}
