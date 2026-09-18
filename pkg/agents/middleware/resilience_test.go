package middleware

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

func TestFallbackRetriesEachTargetAndIsolatesAttempts(t *testing.T) {
	call := &agents.ModelCall{Model: "primary"}
	request := &responses.Request{Model: "primary"}
	var targets []string
	_, err := agents.ExecuteModelCallWithMiddleware(t.Context(), []agents.ModelCallMiddleware{
		NewFallbackModels("Other/backup"), NewRetry(RetryConfig{MaxAttempts: 2, InitialBackoff: time.Nanosecond}),
	}, call, request, func(ctx context.Context, c *agents.ModelCall, r *responses.Request) (*responses.Response, error) {
		targets = append(targets, c.Target)
		require.NotEqual(t, "mutated", r.Model)
		c.Model = "mutated"
		r.Model = "mutated"
		if len(targets) == 4 {
			return agents.ModelCallText("ok"), nil
		}
		return nil, io.ErrUnexpectedEOF
	})
	require.NoError(t, err)
	require.Equal(t, []string{"", "", "Other/backup", "Other/backup"}, targets)
	require.Equal(t, "primary", request.Model)
	require.Equal(t, "primary", call.Model)
}

type failingStreamProvider struct {
	llm.Provider
	calls     int
	committed bool
}

func (p *failingStreamProvider) NewStreamingResponses(context.Context, *responses.Request) (chan *responses.ResponseChunk, error) {
	p.calls++
	ch := make(chan *responses.ResponseChunk, 2)
	if p.committed {
		ch <- &responses.ResponseChunk{OfResponseCreated: &responses.ChunkResponse[constants.ChunkTypeResponseCreated]{}}
	}
	ch <- responses.NewStreamError(errors.New("stream failed"))
	close(ch)
	return ch, nil
}
func TestRetryStreamCommitBoundary(t *testing.T) {
	for _, committed := range []bool{false, true} {
		p := &failingStreamProvider{committed: committed}
		published := 0
		_, err := agents.ExecuteModelCallWithMiddleware(t.Context(), []agents.ModelCallMiddleware{
			NewFallbackModels("Other/backup"), NewRetry(RetryConfig{MaxAttempts: 2, InitialBackoff: time.Nanosecond}),
		}, &agents.ModelCall{}, &responses.Request{}, func(ctx context.Context, call *agents.ModelCall, r *responses.Request) (*responses.Response, error) {
			// For this test both targets use the same fake transport.
			c := *call
			c.Target = ""
			return agents.InvokeModelCall(ctx, p, &c, r, func(*responses.ResponseChunk) { published++ })
		})
		require.Error(t, err)
		require.True(t, agents.IsTerminalModelCallError(err))
		require.Equal(t, committed, agents.ModelStreamCommitted(err))
		if committed {
			require.Equal(t, 1, p.calls)
			require.Equal(t, 1, published)
		} else {
			require.Equal(t, 4, p.calls)
			require.Zero(t, published)
		}
	}
}
func TestRetryBackoffHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	calls := 0
	_, err := agents.ExecuteModelCallWithMiddleware(ctx, []agents.ModelCallMiddleware{NewRetry(RetryConfig{})}, nil, &responses.Request{}, func(context.Context, *agents.ModelCall, *responses.Request) (*responses.Response, error) {
		calls++
		cancel()
		return nil, io.ErrUnexpectedEOF
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, calls)
}
func TestStopCannotBeOverriddenByRetryPolicy(t *testing.T) {
	for _, failure := range []error{agents.ErrModelCallStopped, context.Canceled, context.DeadlineExceeded} {
		calls := 0
		_, err := agents.ExecuteModelCallWithMiddleware(t.Context(), []agents.ModelCallMiddleware{
			NewFallback(FallbackConfig{Targets: []string{"Other/model"}, Fallbackable: func(error) bool { return true }}),
			NewRetry(RetryConfig{Retryable: func(error) bool { return true }}),
		}, nil, &responses.Request{}, func(context.Context, *agents.ModelCall, *responses.Request) (*responses.Response, error) {
			calls++
			return nil, failure
		})
		require.ErrorIs(t, err, failure)
		require.Equal(t, 1, calls)
	}
}

type responseFailure struct{ agents.NoopMiddleware }

func (responseFailure) WrapModelCall(next agents.ModelCallFunc) agents.ModelCallFunc {
	return func(ctx context.Context, call *agents.ModelCall, request *responses.Request) (*responses.Response, error) {
		_, err := next(ctx, call, request)
		if err != nil {
			return nil, err
		}
		return nil, io.ErrUnexpectedEOF
	}
}

type completedProvider struct {
	llm.Provider
	calls int
}

func (p *completedProvider) NewStreamingResponses(context.Context, *responses.Request) (chan *responses.ResponseChunk, error) {
	p.calls++
	ch := make(chan *responses.ResponseChunk, 1)
	ch <- &responses.ResponseChunk{OfResponseCompleted: &responses.ChunkResponse[constants.ChunkTypeResponseCompleted]{}}
	close(ch)
	return ch, nil
}
func TestPostProcessingFailureCannotRestartPublishedResponse(t *testing.T) {
	p := &completedProvider{}
	_, err := agents.ExecuteModelCallWithMiddleware(t.Context(), []agents.ModelCallMiddleware{
		NewFallbackModels("Other/model"), NewRetry(RetryConfig{}), responseFailure{},
	}, nil, &responses.Request{}, func(ctx context.Context, call *agents.ModelCall, request *responses.Request) (*responses.Response, error) {
		return agents.InvokeModelCall(ctx, p, call, request, func(*responses.ResponseChunk) {})
	})
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.True(t, agents.ModelStreamCommitted(err))
	require.Equal(t, 1, p.calls)
}
func TestCancellationDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	retry := NewRetry(RetryConfig{InitialBackoff: time.Hour, Retryable: func(error) bool { cancel(); return true }})
	started := time.Now()
	_, err := agents.ExecuteModelCallWithMiddleware(ctx, []agents.ModelCallMiddleware{retry}, nil, &responses.Request{}, func(context.Context, *agents.ModelCall, *responses.Request) (*responses.Response, error) {
		calls++
		return nil, io.ErrUnexpectedEOF
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, calls)
	require.Less(t, time.Since(started), time.Second)
}
