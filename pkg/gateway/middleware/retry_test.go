package middleware

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testRetry builds a middleware whose backoff is deterministic and instant,
// and records what it would have slept.
func testRetry(cfg RetryConfig) (*Retry, *[]time.Duration) {
	m := NewRetry(cfg)
	slept := &[]time.Duration{}
	m.jitter = func(d time.Duration) time.Duration { return d }
	m.sleep = func(ctx context.Context, d time.Duration) error {
		*slept = append(*slept, d)
		return ctx.Err()
	}
	return m, slept
}

func apiErr(status int) error {
	return &llm.APIError{StatusCode: status, Message: "boom"}
}

func responsesRequest() *llm.Request {
	return &llm.Request{OfResponsesInput: &responses.Request{Model: "gpt-4o-mini"}}
}

// --- classification -------------------------------------------------------

type fakeNetErr struct{ timeout bool }

func (e fakeNetErr) Error() string   { return "net" }
func (e fakeNetErr) Timeout() bool   { return e.timeout }
func (e fakeNetErr) Temporary() bool { return false }

var _ net.Error = fakeNetErr{}

func TestDefaultRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"rate limited", apiErr(429), true},
		{"server error", apiErr(500), true},
		{"unavailable", apiErr(503), true},
		{"request timeout", apiErr(408), true},
		{"bad request", apiErr(400), false},
		{"unauthorized", apiErr(401), false},
		{"not found", apiErr(404), false},
		{"unprocessable", apiErr(422), false},
		{"not implemented", apiErr(501), false},
		{"context cancelled", context.Canceled, false},
		{"deadline exceeded", context.DeadlineExceeded, false},
		{"truncated body", io.ErrUnexpectedEOF, true},
		{"connection reset", syscall.ECONNRESET, true},
		{"network timeout", fakeNetErr{timeout: true}, true},
		{"unknown", errors.New("something else"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, DefaultRetryable(tc.err))
		})
	}
}

func TestDefaultRetryablePrefersCancellationOverNetError(t *testing.T) {
	// A cancelled HTTP request arrives as a *url.Error, which satisfies
	// net.Error while wrapping context.Canceled. Cancellation must win.
	err := &net.OpError{Op: "read", Err: context.Canceled}
	assert.False(t, DefaultRetryable(err))
}

// --- unary ----------------------------------------------------------------

func TestRetryReturnsTheFirstSuccess(t *testing.T) {
	m, slept := testRetry(RetryConfig{})

	calls := 0
	handler := m.HandleRequest(func(context.Context, llm.ProviderName, string, *llm.Request) (*llm.Response, error) {
		calls++
		if calls < 3 {
			return nil, apiErr(503)
		}
		return &llm.Response{}, nil
	})

	resp, err := handler(context.Background(), llm.ProviderNameOpenAI, "k", responsesRequest())

	require.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, 3, calls)
	assert.Equal(t, []time.Duration{500 * time.Millisecond, time.Second}, *slept)
}

func TestRetryStopsAtMaxAttempts(t *testing.T) {
	m, _ := testRetry(RetryConfig{MaxAttempts: 3})

	calls := 0
	handler := m.HandleRequest(func(context.Context, llm.ProviderName, string, *llm.Request) (*llm.Response, error) {
		calls++
		return nil, apiErr(503)
	})

	_, err := handler(context.Background(), llm.ProviderNameOpenAI, "k", responsesRequest())

	require.Error(t, err)
	assert.Equal(t, 503, llm.StatusCodeOf(err))
	assert.Equal(t, 3, calls, "MaxAttempts counts the first call")
}

func TestRetryLeavesClientErrorsAlone(t *testing.T) {
	m, slept := testRetry(RetryConfig{})

	calls := 0
	handler := m.HandleRequest(func(context.Context, llm.ProviderName, string, *llm.Request) (*llm.Response, error) {
		calls++
		return nil, apiErr(400)
	})

	_, err := handler(context.Background(), llm.ProviderNameOpenAI, "k", responsesRequest())

	require.Error(t, err)
	assert.Equal(t, 1, calls)
	assert.Empty(t, *slept)
}

func TestRetryObeysRetryAfter(t *testing.T) {
	m, slept := testRetry(RetryConfig{})

	calls := 0
	handler := m.HandleRequest(func(context.Context, llm.ProviderName, string, *llm.Request) (*llm.Response, error) {
		calls++
		if calls == 1 {
			return nil, &llm.APIError{StatusCode: 429, Message: "slow down", RetryAfter: 7 * time.Second}
		}
		return &llm.Response{}, nil
	})

	_, err := handler(context.Background(), llm.ProviderNameOpenAI, "k", responsesRequest())

	require.NoError(t, err)
	assert.Equal(t, []time.Duration{7 * time.Second}, *slept,
		"a provider that named a delay is obeyed rather than jittered")
}

func TestRetryCapsExponentialBackoff(t *testing.T) {
	m, slept := testRetry(RetryConfig{
		MaxAttempts:    5,
		InitialBackoff: time.Second,
		MaxBackoff:     3 * time.Second,
	})

	handler := m.HandleRequest(func(context.Context, llm.ProviderName, string, *llm.Request) (*llm.Response, error) {
		return nil, apiErr(503)
	})
	_, _ = handler(context.Background(), llm.ProviderNameOpenAI, "k", responsesRequest())

	assert.Equal(t, []time.Duration{time.Second, 2 * time.Second, 3 * time.Second, 3 * time.Second}, *slept)
}

func TestRetryDisabledByMaxAttemptsOne(t *testing.T) {
	m, _ := testRetry(RetryConfig{MaxAttempts: 1})

	calls := 0
	handler := m.HandleRequest(func(context.Context, llm.ProviderName, string, *llm.Request) (*llm.Response, error) {
		calls++
		return nil, apiErr(503)
	})
	_, err := handler(context.Background(), llm.ProviderNameOpenAI, "k", responsesRequest())

	require.Error(t, err)
	assert.Equal(t, 1, calls)
}

func TestRetryStopsWhenTheContextEnds(t *testing.T) {
	m, _ := testRetry(RetryConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	calls := 0
	handler := m.HandleRequest(func(context.Context, llm.ProviderName, string, *llm.Request) (*llm.Response, error) {
		calls++
		return nil, apiErr(503)
	})
	_, err := handler(ctx, llm.ProviderNameOpenAI, "k", responsesRequest())

	require.Error(t, err)
	assert.Equal(t, 503, llm.StatusCodeOf(err), "the provider's failure is reported, not the wait's")
	assert.Equal(t, 1, calls)
}

// --- streaming ------------------------------------------------------------

func textChunk(delta string) *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfOutputTextDelta: &responses.ChunkOutputText[constants.ChunkTypeOutputTextDelta]{Delta: delta},
	}
}

func errorChunk(msg string) *responses.ResponseChunk {
	return &responses.ResponseChunk{OfError: &responses.StreamError{Type: "error", Message: msg}}
}

// stream produces chunks on an unbuffered channel and closes it. done is
// closed once the producer has finished, which is how the tests prove an
// abandoned attempt was drained rather than leaked.
func stream(chunks ...*responses.ResponseChunk) (chan *responses.ResponseChunk, chan struct{}) {
	ch := make(chan *responses.ResponseChunk)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(ch)
		for _, c := range chunks {
			ch <- c
		}
	}()
	return ch, done
}

func collect(t *testing.T, ch <-chan *responses.ResponseChunk) []*responses.ResponseChunk {
	t.Helper()
	var got []*responses.ResponseChunk
	for {
		select {
		case c, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, c)
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for the stream to close")
		}
	}
}

func TestRetryStreamRetriesBeforeTheStreamOpens(t *testing.T) {
	m, _ := testRetry(RetryConfig{})

	calls := 0
	handler := m.HandleStreamingRequest(func(context.Context, llm.ProviderName, string, *llm.Request) (*llm.StreamingResponse, error) {
		calls++
		if calls == 1 {
			return nil, apiErr(429)
		}
		ch, _ := stream(textChunk("hello"))
		return &llm.StreamingResponse{ResponsesStreamData: ch}, nil
	})

	resp, err := handler(context.Background(), llm.ProviderNameOpenAI, "k", responsesRequest())

	require.NoError(t, err)
	got := collect(t, resp.ResponsesStreamData)
	require.Len(t, got, 1)
	assert.Equal(t, "hello", got[0].OfOutputTextDelta.Delta)
	assert.Equal(t, 2, calls)
}

func TestRetryStreamRetriesAnErrorInTheFirstChunk(t *testing.T) {
	m, _ := testRetry(RetryConfig{})

	calls := 0
	var firstDone chan struct{}
	handler := m.HandleStreamingRequest(func(context.Context, llm.ProviderName, string, *llm.Request) (*llm.StreamingResponse, error) {
		calls++
		if calls == 1 {
			ch, done := stream(errorChunk("overloaded"))
			firstDone = done
			return &llm.StreamingResponse{ResponsesStreamData: ch}, nil
		}
		ch, _ := stream(textChunk("recovered"))
		return &llm.StreamingResponse{ResponsesStreamData: ch}, nil
	})

	resp, err := handler(context.Background(), llm.ProviderNameOpenAI, "k", responsesRequest())
	require.NoError(t, err)

	got := collect(t, resp.ResponsesStreamData)
	require.Len(t, got, 1, "the abandoned attempt must not reach the caller")
	assert.Equal(t, "recovered", got[0].OfOutputTextDelta.Delta)
	assert.Equal(t, 2, calls)

	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the abandoned attempt was not drained")
	}
}

func TestRetryStreamCommitsOnceAChunkIsDelivered(t *testing.T) {
	m, _ := testRetry(RetryConfig{})

	calls := 0
	handler := m.HandleStreamingRequest(func(context.Context, llm.ProviderName, string, *llm.Request) (*llm.StreamingResponse, error) {
		calls++
		ch, _ := stream(textChunk("par"), errorChunk("died mid-flight"))
		return &llm.StreamingResponse{ResponsesStreamData: ch}, nil
	})

	resp, err := handler(context.Background(), llm.ProviderNameOpenAI, "k", responsesRequest())
	require.NoError(t, err)

	got := collect(t, resp.ResponsesStreamData)
	require.Len(t, got, 2)
	assert.Equal(t, "par", got[0].OfOutputTextDelta.Delta)
	require.NotNil(t, got[1].OfError)
	assert.Equal(t, 1, calls, "a half-delivered stream cannot be retried")
}

func TestRetryStreamGivesUpAndReportsTheFailure(t *testing.T) {
	m, _ := testRetry(RetryConfig{MaxAttempts: 2})

	calls := 0
	handler := m.HandleStreamingRequest(func(context.Context, llm.ProviderName, string, *llm.Request) (*llm.StreamingResponse, error) {
		calls++
		ch, _ := stream(errorChunk("still overloaded"))
		return &llm.StreamingResponse{ResponsesStreamData: ch}, nil
	})

	resp, err := handler(context.Background(), llm.ProviderNameOpenAI, "k", responsesRequest())
	require.NoError(t, err)

	got := collect(t, resp.ResponsesStreamData)
	require.Len(t, got, 1)
	require.NotNil(t, got[0].OfError)
	assert.Equal(t, "still overloaded", got[0].OfError.Message)
	assert.Equal(t, 2, calls)
}

func TestRetryStreamReportsAFailedRetry(t *testing.T) {
	m, _ := testRetry(RetryConfig{})

	calls := 0
	handler := m.HandleStreamingRequest(func(context.Context, llm.ProviderName, string, *llm.Request) (*llm.StreamingResponse, error) {
		calls++
		if calls == 1 {
			ch, _ := stream(errorChunk("overloaded"))
			return &llm.StreamingResponse{ResponsesStreamData: ch}, nil
		}
		return nil, apiErr(400)
	})

	resp, err := handler(context.Background(), llm.ProviderNameOpenAI, "k", responsesRequest())
	require.NoError(t, err)

	got := collect(t, resp.ResponsesStreamData)
	require.Len(t, got, 1)
	require.NotNil(t, got[0].OfError, "the caller must learn the retry failed")
}

func TestRetryStreamHonoursTheStreamErrorPolicy(t *testing.T) {
	m, _ := testRetry(RetryConfig{
		RetryableStreamError: func(e *responses.StreamError) bool { return e.Code == "overloaded_error" },
	})

	calls := 0
	handler := m.HandleStreamingRequest(func(context.Context, llm.ProviderName, string, *llm.Request) (*llm.StreamingResponse, error) {
		calls++
		ch, _ := stream(errorChunk("invalid request"))
		return &llm.StreamingResponse{ResponsesStreamData: ch}, nil
	})

	resp, err := handler(context.Background(), llm.ProviderNameOpenAI, "k", responsesRequest())
	require.NoError(t, err)

	collect(t, resp.ResponsesStreamData)
	assert.Equal(t, 1, calls, "a policy that rejects the error must prevent the retry")
}

func TestRetryStreamPassesNonResponsesStreamsThrough(t *testing.T) {
	m, _ := testRetry(RetryConfig{})

	handler := m.HandleStreamingRequest(func(context.Context, llm.ProviderName, string, *llm.Request) (*llm.StreamingResponse, error) {
		return &llm.StreamingResponse{}, nil
	})

	resp, err := handler(context.Background(), llm.ProviderNameOpenAI, "k", responsesRequest())
	require.NoError(t, err)
	assert.Nil(t, resp.ResponsesStreamData)
}
