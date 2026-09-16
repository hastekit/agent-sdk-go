package agents

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

type modelPublicationKey struct{}

type modelStreamTransformKey struct{}

// ModelStreamTransform can copy, replace, or suppress one provider stream
// chunk before it is published. Returning nil suppresses the chunk. Returning
// an error ends and drains the attempt; the error is classified as a middleware
// failure, not a provider failure.
type ModelStreamTransform func(context.Context, *responses.ResponseChunk) (*responses.ResponseChunk, error)

// WithModelStreamTransform installs a response-side transform for the model
// call beneath it. Multiple transforms compose in middleware response order:
// the innermost transform runs first. Transforms must not mutate provider chunks.
func WithModelStreamTransform(ctx context.Context, transform ModelStreamTransform) context.Context {
	if transform == nil {
		return ctx
	}
	previous, _ := ctx.Value(modelStreamTransformKey{}).(ModelStreamTransform)
	if previous == nil {
		return context.WithValue(ctx, modelStreamTransformKey{}, transform)
	}
	combined := func(ctx context.Context, chunk *responses.ResponseChunk) (*responses.ResponseChunk, error) {
		chunk, err := transform(ctx, chunk)
		if err != nil || chunk == nil {
			return chunk, err
		}
		return previous(ctx, chunk)
	}
	return context.WithValue(ctx, modelStreamTransformKey{}, ModelStreamTransform(combined))
}

type modelStreamTransformError struct{ cause error }

func (e *modelStreamTransformError) Error() string { return e.cause.Error() }
func (e *modelStreamTransformError) Unwrap() error { return e.cause }

// IsModelStreamTransformError reports an error raised while middleware was
// transforming a live model chunk. Resilience middleware must not retry it.
func IsModelStreamTransformError(err error) bool {
	var target *modelStreamTransformError
	return errors.As(err, &target)
}

func streamTransformError(err error) error {
	if err == nil || IsModelStreamTransformError(err) {
		return err
	}
	return &modelStreamTransformError{cause: err}
}

// Tracks whether this call has already published a stream chunk.
func guardModelPublication(next ModelCallFunc) ModelCallFunc {
	return func(ctx context.Context, call *ModelCall, request *responses.Request) (*responses.Response, error) {
		response, err := next(ctx, call, request)
		if committed, ok := ctx.Value(modelPublicationKey{}).(*atomic.Bool); err != nil && ok && committed.Load() && !ModelStreamCommitted(err) {
			return nil, &ModelCallError{Cause: err, StreamCommitted: true}
		}
		return response, err
	}
}

// ModelCallError tells durable runtimes whether a call must not be retried.
// The original error remains available through errors.Is and errors.As.
type ModelCallError struct {
	Cause           error
	StreamCommitted bool
	PolicyFinished  bool
}

func (e *ModelCallError) Error() string { return e.Cause.Error() }
func (e *ModelCallError) Unwrap() error { return e.Cause }

func ModelStreamCommitted(err error) bool {
	var e *ModelCallError
	return errors.As(err, &e) && e.StreamCommitted
}

// IsTerminalModelCallError reports whether a durable runtime must not run the
// call again. This is true after output was published, a policy was exhausted,
// or middleware returned its own error.
func IsTerminalModelCallError(err error) bool {
	var e *ModelCallError
	return (errors.As(err, &e) && (e.StreamCommitted || e.PolicyFinished)) || IsModelCallAborted(err)
}

// ErrModelCallAborted marks an error returned by model middleware itself.
// Durable runtimes use it to avoid retrying deterministic failures such as
// missing attachments or denied access. The original error remains unwrap-able.
var ErrModelCallAborted = errors.New("model call aborted by middleware")

// IsModelCallAborted reports whether middleware returned the error.
func IsModelCallAborted(err error) bool {
	return errors.Is(err, ErrModelCallAborted)
}

func abortedModelCall(err error) error {
	if err == nil {
		return ErrModelCallAborted
	}
	if IsModelCallAborted(err) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrModelCallAborted, err)
}

// providerFailure marks errors produced by the provider while they pass through
// middleware. This lets us distinguish them from errors created by middleware.
type providerFailure struct{ err error }

func (f *providerFailure) Error() string { return f.err.Error() }
func (f *providerFailure) Unwrap() error { return f.err }

// modelCallOutcome classifies an error after the middleware chain completes.
// Provider errors pass through; middleware errors are marked as terminal.
func modelCallOutcome(ctx context.Context, err error) error {
	var failure *providerFailure
	if errors.As(err, &failure) {
		if err == failure {
			// Nothing added to it on the way out; hand back the original.
			return failure.err
		}
		// A middleware said something about it. Keep that, and the identity
		// underneath for errors.Is.
		return err
	}
	// A stop, or the caller going away, is control flow rather than a
	// policy refusal.
	if errors.Is(err, ErrModelCallStopped) || (ctx.Err() != nil && errors.Is(err, ctx.Err())) {
		return err
	}
	return abortedModelCall(err)
}

// ModelTargetProvider supports explicit per-call routing without changing the
// provider bound to an agent. Target uses Provider/model notation.
type ModelTargetProvider interface {
	NewStreamingResponsesForModel(context.Context, string, *responses.Request) (chan *responses.ResponseChunk, error)
}

// InvokeModelCall calls the provider with a request copy and records whether
// any stream chunk was published.
func InvokeModelCall(ctx context.Context, provider llm.Provider, call *ModelCall, request *responses.Request, publish func(*responses.ResponseChunk)) (*responses.Response, error) {
	prepared := *request
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var stream chan *responses.ResponseChunk
	var err error
	if call != nil && call.Target != "" {
		router, ok := provider.(ModelTargetProvider)
		if !ok {
			return nil, &ModelCallError{Cause: fmt.Errorf("model provider does not support explicit target %q", call.Target), PolicyFinished: true}
		}
		stream, err = router.NewStreamingResponsesForModel(attemptCtx, call.Target, &prepared)
	} else {
		stream, err = provider.NewStreamingResponses(attemptCtx, &prepared)
	}
	if err != nil {
		return nil, err
	}
	committed := false
	acc := Accumulator{}
	transform, _ := attemptCtx.Value(modelStreamTransformKey{}).(ModelStreamTransform)
	response, err := acc.readStream(attemptCtx, stream, func(chunk *responses.ResponseChunk) error {
		published := chunk
		if transform != nil {
			var transformErr error
			published, transformErr = transform(attemptCtx, chunk)
			if transformErr != nil {
				return streamTransformError(transformErr)
			}
		}
		if published == nil {
			return nil
		}
		committed = true
		if state, ok := attemptCtx.Value(modelPublicationKey{}).(*atomic.Bool); ok {
			state.Store(true)
		}
		publish(published)
		return nil
	})
	if err != nil && committed {
		return nil, &ModelCallError{Cause: err, StreamCommitted: true}
	}
	return response, err
}
