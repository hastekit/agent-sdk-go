package middleware

import (
	"context"
	"errors"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	gatewaymiddleware "github.com/hastekit/agent-sdk-go/pkg/gateway/middleware"
)

// RetryConfig shares gateway retry budgets, classification and backoff options.
type RetryConfig = gatewaymiddleware.RetryConfig

// Retry retries model attempts only before any chunk has been published.
// Install inside Fallback so each target receives its own attempt budget.
type Retry struct {
	agents.NoopMiddleware
	cfg RetryConfig
}

var _ agents.Middleware = (*Retry)(nil)

func NewRetry(cfg RetryConfig) *Retry {
	if cfg.MaxAttempts < 1 {
		cfg.MaxAttempts = gatewaymiddleware.DefaultMaxAttempts
	}
	if cfg.Retryable == nil {
		cfg.Retryable = gatewaymiddleware.DefaultRetryable
	}
	if cfg.RetryableStreamError == nil {
		cfg.RetryableStreamError = func(*responses.StreamError) bool { return true }
	}
	return &Retry{cfg: cfg}
}
func (m *Retry) WrapModelCall(next agents.ModelCallFunc) agents.ModelCallFunc {
	return func(ctx context.Context, call *agents.ModelCall, request *responses.Request) (*responses.Response, error) {
		for attempt := 1; ; attempt++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			c, r := *call, *request
			response, err := next(ctx, &c, &r)
			if err == nil {
				return response, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if cannotRepeat(ctx, err) || attempt >= m.cfg.MaxAttempts || !m.retryable(err) {
				return nil, policyFinished(err)
			}
			timer := time.NewTimer(m.cfg.Backoff(attempt, err))
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
	}
}
func (m *Retry) retryable(err error) bool {
	var streamErr *responses.StreamError
	if errors.As(err, &streamErr) {
		return m.cfg.RetryableStreamError(streamErr)
	}
	return m.cfg.Retryable(err)
}
func cannotRepeat(ctx context.Context, err error) bool {
	return ctx.Err() != nil || agents.IsModelStreamTransformError(err) || agents.ModelStreamCommitted(err) || errors.Is(err, agents.ErrModelCallStopped) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
func policyFinished(err error) error {
	return &agents.ModelCallError{Cause: err, PolicyFinished: true, StreamCommitted: agents.ModelStreamCommitted(err)}
}
