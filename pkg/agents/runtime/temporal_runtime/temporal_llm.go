package temporal_runtime

import (
	"context"
	"log/slog"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// ModelCallFailedErrorType marks an LLM activity failure Temporal must not
// retry: a retry or fallback policy that ran to its end, a stream that had
// already reached the client, or a middleware's own error, which would only be
// refused again — see agents.IsTerminalModelCallError.
const ModelCallFailedErrorType = "ModelCallFailed"

type TemporalLLM struct {
	wrappedLLM llm.Provider
	broker     agents.StreamBroker

	// middlewares are the agent's real model-call middlewares, whose wraps run here,
	// inside the activity.
	middlewares []agents.ModelCallMiddleware
}

func NewTemporalLLM(wrappedLLM llm.Provider, broker agents.StreamBroker, middlewares ...agents.ModelCallMiddleware) *TemporalLLM {
	return &TemporalLLM{
		wrappedLLM:  wrappedLLM,
		broker:      broker,
		middlewares: middlewares,
	}
}

// NewStreamingResponsesActivity is where the model is really called, so it is
// also where a stop has to reach it: cancelling from the workflow would end the
// wait and leave the provider streaming tokens nobody wants. The stream channel
// is the workflow execution id, the same one the loop stops on and the same one
// chunks are published to.
//
// It is also where the middlewares' WrapModelCall runs: the request has crossed
// into the activity in the shape history keeps it, with the call it belongs
// to, and whatever the wraps hand the provider — the bytes behind an attachment file_id —
// is sent from here and journaled nowhere.
func (l *TemporalLLM) NewStreamingResponsesActivity(ctx context.Context, in *responses.Request, call *agents.ModelCall) (*responses.Response, error) {
	streamID := activity.GetInfo(ctx).WorkflowExecution.ID

	middlewares := append([]agents.ModelCallMiddleware{agents.StopMiddleware{Watcher: agents.StopWatcherFrom(l.broker), StreamID: streamID}}, l.middlewares...)

	resp, err := agents.ExecuteModelCallWithMiddleware(ctx, middlewares, call, in, func(ctx context.Context, call *agents.ModelCall, in *responses.Request) (*responses.Response, error) {
		return agents.InvokeModelCall(ctx, l.wrappedLLM, call, in, func(chunk *responses.ResponseChunk) {
			if err := l.broker.Publish(ctx, streamID, chunk); err != nil {
				slog.ErrorContext(ctx, "Failed to publish chunk to stream broker", "error", err)
			}
		})
	})
	if err != nil {
		// Completed resilience policies, published streams and a
		// middleware's own refusal must not be restarted by activity retries.
		// Stops retain their own error type; a provider's failure stays
		// retryable, as any activity failure is.
		if !stoppedWork(err) && agents.IsTerminalModelCallError(err) {
			return nil, temporal.NewNonRetryableApplicationError(err.Error(), ModelCallFailedErrorType, nil)
		}
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

func (l *TemporalLLMProxy) NewStreamingResponses(ctx context.Context, call *agents.ModelCall, in *responses.Request, cb func(chunk *responses.ResponseChunk)) (*responses.Response, error) {
	var response *responses.Response
	// The call goes with the request, for the wraps on the activity side.
	err := workflow.ExecuteActivity(l.workflowCtx, l.prefix+"_NewStreamingResponsesActivity", in, call).Get(l.workflowCtx, &response)
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
