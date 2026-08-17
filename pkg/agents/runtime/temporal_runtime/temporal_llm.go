package temporal_runtime

import (
	"context"
	"log/slog"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/workflow"
)

type TemporalLLM struct {
	wrappedLLM llm.Provider
	broker     agents.StreamBroker
}

func NewTemporalLLM(wrappedLLM llm.Provider, broker agents.StreamBroker) *TemporalLLM {
	return &TemporalLLM{
		wrappedLLM: wrappedLLM,
		broker:     broker,
	}
}

// NewStreamingResponsesActivity is where the model is really called, so it is
// also where a stop has to reach it: cancelling from the workflow would end the
// wait and leave the provider streaming tokens nobody wants. The stream channel
// is the workflow execution id, the same one the loop stops on and the same one
// chunks are published to.
func (l *TemporalLLM) NewStreamingResponsesActivity(ctx context.Context, in *responses.Request) (*responses.Response, error) {
	streamID := activity.GetInfo(ctx).WorkflowExecution.ID

	ctx, cancel := agents.StopCancelContext(ctx, agents.StopWatcherFrom(l.broker), streamID)
	defer cancel()

	stream, err := l.wrappedLLM.NewStreamingResponses(ctx, in)
	if err != nil {
		return nil, cancellationError(err)
	}

	acc := agents.Accumulator{}
	resp, err := acc.ReadStream(ctx, stream, func(chunk *responses.ResponseChunk) {
		if err := l.broker.Publish(ctx, streamID, chunk); err != nil {
			slog.ErrorContext(ctx, "Failed to publish chunk to stream broker", "error", err)
		}
	})
	if err != nil {
		// Non-retryable, or Temporal calls the model again — the one thing a
		// user who pressed stop must not get.
		return nil, cancellationError(err)
	}

	return resp, nil
}

type TemporalLLMProxy struct {
	workflowCtx workflow.Context
	prefix      string
	broker      agents.StreamBroker
}

func NewTemporalLLMProxy(workflowCtx workflow.Context, prefix string, broker agents.StreamBroker) agents.LLM {
	return &TemporalLLMProxy{
		workflowCtx: workflowCtx,
		prefix:      prefix,
		broker:      broker,
	}
}

func (l *TemporalLLMProxy) NewStreamingResponses(ctx context.Context, in *responses.Request, cb func(chunk *responses.ResponseChunk)) (*responses.Response, error) {
	var response *responses.Response
	err := workflow.ExecuteActivity(l.workflowCtx, l.prefix+"_NewStreamingResponsesActivity", in).Get(l.workflowCtx, &response)
	if err != nil {
		// A call the stop cut short comes back as an activity failure. Report it
		// as the stop it is, so the loop ends the run cleanly instead of
		// failing it — the error type is all that survives the boundary.
		if wasCancelled(err) {
			return nil, agents.ErrModelCallStopped
		}
		return nil, err
	}

	return response, nil
}
