package middleware

import (
	"context"
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"syscall"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/gateway"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// Defaults for RetryConfig. Three attempts is the same budget the official
// OpenAI and Anthropic clients use: enough to ride out a transient 429 or a
// dropped connection, few enough that a genuinely broken provider fails while
// the caller is still watching.
const (
	DefaultMaxAttempts    = 3
	DefaultInitialBackoff = 500 * time.Millisecond
	DefaultMaxBackoff     = 30 * time.Second
	DefaultMultiplier     = 2.0
)

// RetryConfig tunes Retry. The zero value is usable: every field
// falls back to its Default above.
type RetryConfig struct {
	// MaxAttempts counts the first attempt, so 3 means one call and two
	// retries. Values below 1 select DefaultMaxAttempts; exactly 1 disables
	// retrying without removing the middleware.
	MaxAttempts int

	// InitialBackoff is the wait before the second attempt. Each further
	// attempt multiplies it by Multiplier, up to MaxBackoff, and the result
	// is jittered.
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	Multiplier     float64

	// Retryable decides whether an error is worth another attempt. Nil
	// selects DefaultRetryable.
	Retryable func(error) bool

	// RetryableStreamError decides whether a stream that failed before
	// delivering anything is worth another attempt. Nil retries all of them:
	// see Retry's note on why that is safe but imprecise.
	RetryableStreamError func(*responses.StreamError) bool
}

func (c RetryConfig) withDefaults() RetryConfig {
	if c.MaxAttempts < 1 {
		c.MaxAttempts = DefaultMaxAttempts
	}
	if c.InitialBackoff <= 0 {
		c.InitialBackoff = DefaultInitialBackoff
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = DefaultMaxBackoff
	}
	if c.Multiplier < 1 {
		c.Multiplier = DefaultMultiplier
	}
	if c.Retryable == nil {
		c.Retryable = DefaultRetryable
	}
	if c.RetryableStreamError == nil {
		c.RetryableStreamError = func(*responses.StreamError) bool { return true }
	}
	return c
}

// Retry re-issues a gateway request that failed for a reason another attempt
// could plausibly fix — a rate limit, a 5xx, a dropped connection.
//
// Install it before Tracing so each attempt gets its own span; a call that
// took three tries then reads as three spans rather than one slow one:
//
//	gw.UseMiddleware(middleware.NewRetry(cfg), middleware.NewTracing())
//
// Streaming is retried only up to the first chunk the caller sees. A stream
// that fails before delivering anything is indistinguishable from one that
// never opened, so it is retried; once a chunk has been forwarded the attempt
// is committed, because there is no way to un-send it. That boundary is the
// honest one — resuming a half-delivered stream would need the provider to
// support replay, and none of them do.
//
// In-band stream failures carry no status code (they arrive as an SSE error
// event, flattened to a message), so by default every one of them is retried.
// Retrying is always *safe* there — nothing was delivered — but for a
// permanent error it is merely wasted, bounded by MaxAttempts. Set
// RetryConfig.RetryableStreamError to narrow it.
type Retry struct {
	cfg RetryConfig

	// jitter spreads retries from concurrent callers so they do not
	// synchronize into a second thundering herd. Tests replace it.
	jitter func(time.Duration) time.Duration

	// sleep is the delay itself, ctx-aware. Tests replace it.
	sleep func(context.Context, time.Duration) error
}

var _ gateway.Middleware = (*Retry)(nil)

// NewRetry returns a Retry using cfg, with every unset field defaulted.
func NewRetry(cfg RetryConfig) *Retry {
	return &Retry{
		cfg:    cfg.withDefaults(),
		jitter: fullJitter,
		sleep:  sleepContext,
	}
}

// DefaultRetryable reports whether err is worth another attempt.
//
// Retryable: 408, 409, 425, 429 and the 5xx codes that mean "not now" (500,
// 502, 503, 504), plus transport failures — timeouts, resets, truncated
// bodies. Not retryable: a cancelled context, and every 4xx that describes
// the request itself, since the same request will fail the same way.
func DefaultRetryable(err error) bool {
	if err == nil {
		return false
	}

	// Cancellation first: a cancelled request surfaces as a *url.Error, which
	// also satisfies net.Error, and retrying the caller's own cancellation
	// would be perverse.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	if apiErr, ok := llm.AsAPIError(err); ok {
		return RetryableStatus(apiErr.StatusCode)
	}

	if errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EPIPE) {
		return true
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}

	return false
}

// RetryableStatus reports whether an HTTP status is worth another attempt.
func RetryableStatus(code int) bool {
	switch code {
	case 408, // request timeout
		409, // conflict; providers use it for transient lock contention
		425, // too early
		429, // rate limited
		500, // internal error
		502, // bad gateway
		503, // unavailable
		504: // gateway timeout
		return true
	default:
		return false
	}
}

func (m *Retry) HandleRequest(next gateway.RequestHandler) gateway.RequestHandler {
	return func(ctx context.Context, providerName llm.ProviderName, key string, r *llm.Request) (*llm.Response, error) {
		var resp *llm.Response
		var err error

		for attempt := 1; ; attempt++ {
			resp, err = next(ctx, providerName, key, r)
			if err == nil {
				return resp, nil
			}
			if !m.shouldRetry(attempt, err) {
				return resp, err
			}
			if waitErr := m.wait(ctx, attempt, err); waitErr != nil {
				// The context ended while backing off. Report the provider's
				// failure, not the wait's: the provider is what went wrong.
				return resp, err
			}
		}
	}
}

func (m *Retry) HandleStreamingRequest(next gateway.StreamingRequestHandler) gateway.StreamingRequestHandler {
	return func(ctx context.Context, providerName llm.ProviderName, key string, r *llm.Request) (*llm.StreamingResponse, error) {
		attempt := 1
		resp, err := next(ctx, providerName, key, r)

		// A failure before the stream opens is an ordinary error: no chunk has
		// been produced, so this is the same case as HandleRequest.
		for err != nil {
			if !m.shouldRetry(attempt, err) {
				return resp, err
			}
			if waitErr := m.wait(ctx, attempt, err); waitErr != nil {
				return resp, err
			}
			attempt++
			resp, err = next(ctx, providerName, key, r)
		}

		// Only the Responses stream reports failures in band. Everything else
		// is handed back exactly as the provider gave it.
		if resp == nil || resp.ResponsesStreamData == nil {
			return resp, nil
		}

		// Return immediately and retry from inside the pump, so the caller
		// waits no longer than it did before this middleware existed.
		orig := resp.ResponsesStreamData
		out := make(chan *responses.ResponseChunk)
		resp.ResponsesStreamData = out
		go pumpStream(ctx, out, orig, m.nextAttempt(next, providerName, key, r, &attempt))

		return resp, nil
	}
}

// nextAttempt supplies the retry half of pumpStream: another attempt while
// the budget and the policy both allow one.
func (m *Retry) nextAttempt(
	next gateway.StreamingRequestHandler,
	providerName llm.ProviderName,
	key string,
	r *llm.Request,
	attempt *int,
) nextStreamFn {
	return func(ctx context.Context, prev *responses.StreamError) (<-chan *responses.ResponseChunk, *responses.ResponseChunk, bool) {
		if *attempt >= m.cfg.MaxAttempts || !m.cfg.RetryableStreamError(prev) {
			return nil, nil, false
		}
		if err := m.wait(ctx, *attempt, prev); err != nil {
			return nil, nil, false
		}
		*attempt++

		resp, err := next(ctx, providerName, key, r)
		if err != nil {
			return nil, responses.NewStreamError(err), true
		}
		if resp == nil || resp.ResponsesStreamData == nil {
			return nil, nil, false
		}
		return resp.ResponsesStreamData, nil, true
	}
}

func (m *Retry) shouldRetry(attempt int, err error) bool {
	return attempt < m.cfg.MaxAttempts && m.cfg.Retryable(err)
}

// wait sleeps for the backoff owed after a failed attempt, returning ctx's
// error if the context ended first.
func (m *Retry) wait(ctx context.Context, attempt int, err error) error {
	return m.sleep(ctx, m.backoff(attempt, err))
}

// backoff is the delay after attempt. A provider that named a delay is
// obeyed as given — it knows when its own limit resets, and jittering an
// explicit instruction only invites a second rejection.
func (m *Retry) backoff(attempt int, err error) time.Duration {
	if apiErr, ok := llm.AsAPIError(err); ok && apiErr.RetryAfter > 0 {
		return apiErr.RetryAfter
	}

	d := float64(m.cfg.InitialBackoff) * math.Pow(m.cfg.Multiplier, float64(attempt-1))
	if d > float64(m.cfg.MaxBackoff) {
		d = float64(m.cfg.MaxBackoff)
	}
	return m.jitter(time.Duration(d))
}

// fullJitter picks uniformly from [d/2, d]. Half the delay stays fixed so a
// backoff never collapses to nothing, and the rest spreads concurrent callers.
func fullJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
