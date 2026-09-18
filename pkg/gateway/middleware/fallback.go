package middleware

import (
	"context"
	"errors"
	"fmt"

	"github.com/hastekit/agent-sdk-go/pkg/gateway"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// FallbackTarget is one provider and model to try when the ones before it in
// the chain have failed.
type FallbackTarget struct {
	// Provider names the provider to call. Its API key is resolved from the
	// gateway's gateway.ConfigStore, so a target can be a provider the caller never
	// configured a key for at the call site.
	Provider llm.ProviderName

	// Model is the model to request from Provider. Empty keeps the model the
	// caller asked for, which is what a target for the same model on a
	// different provider wants (an OpenAI-compatible mirror, say).
	Model string
}

func (t FallbackTarget) String() string {
	if t.Model == "" {
		return string(t.Provider)
	}
	return string(t.Provider) + "/" + t.Model
}

// FallbackConfig tunes Fallback.
type FallbackConfig struct {
	// Targets are tried in order after the caller's own provider fails.
	// An empty list makes the middleware a no-op.
	Targets []FallbackTarget

	// Fallbackable decides whether an error should move to the next target.
	// Nil selects DefaultFallbackable.
	Fallbackable func(error) bool
}

// Fallback re-issues a failed request against a different provider.
//
// It belongs outside Retry, so that each target is retried on its
// own before the chain moves on — a 503 that clears on the second attempt
// should never cost you a switch to a weaker model:
//
//	gw.UseMiddleware(
//	    middleware.NewFallback(cfg),   // outermost: changes provider
//	    middleware.NewRetry(retryCfg), // retries within one provider
//	    middleware.NewTracing(),       // one span per attempt
//	)
//
// Falling back changes which model answered, which the caller did not ask
// for. That is the right trade for an outage and the wrong one for a
// deterministic evaluation, so the chain is configuration rather than a
// default: a client installs no middleware it was not given.
//
// Streaming follows the same commit rule as retrying — a stream that fails
// before delivering a chunk can move to the next target; one that has already
// delivered anything cannot, because half an answer from one model followed
// by half from another is worse than a clean failure.
type Fallback struct {
	store gateway.ConfigStore
	cfg   FallbackConfig
}

var (
	_ gateway.Middleware       = (*Fallback)(nil)
	_ gateway.ConfigStoreAware = (*Fallback)(nil)
)

// NewFallback returns a Fallback for cfg.
//
// It needs no store: LLMGateway.UseMiddleware hands it the provider
// configuration on the way in, via ConfigStoreAware. Pass one explicitly with
// WithConfigStore only when composing a chain that no gateway will install.
func NewFallback(cfg FallbackConfig) *Fallback {
	if cfg.Fallbackable == nil {
		cfg.Fallbackable = DefaultFallbackable
	}
	return &Fallback{cfg: cfg}
}

// NewFallbackModels is NewFallback over a chain written in the same
// "Provider/model" notation LLMClient.Model takes:
//
//	middleware.NewFallbackModels("Anthropic/claude-sonnet-4-5", "Gemini/gemini-2.5-flash")
func NewFallbackModels(ids ...string) *Fallback {
	return NewFallback(FallbackConfig{Targets: FallbackModels(ids...)})
}

// FallbackModels parses a chain written as "Provider/model" ids. An id naming
// only a provider — "OpenRouter" — keeps the model the caller asked for and
// only redirects the provider.
func FallbackModels(ids ...string) []FallbackTarget {
	targets := make([]FallbackTarget, 0, len(ids))
	for _, id := range ids {
		provider, model := llm.ParseModelID(id)
		targets = append(targets, FallbackTarget{Provider: provider, Model: model})
	}
	return targets
}

// WithConfigStore implements gateway.ConfigStoreAware. A store set by the
// caller wins; injection only fills a gap.
func (m *Fallback) WithConfigStore(store gateway.ConfigStore) gateway.Middleware {
	bound := *m
	if bound.store == nil {
		bound.store = store
	}
	return &bound
}

// DefaultFallbackable reports whether err is worth trying elsewhere.
//
// Almost everything is: a rate limit, an outage, an expired key, a model that
// this provider has retired. The exceptions are a cancelled context and the
// two statuses that describe the request itself — 400 and 422 — since a
// request the provider could not parse will not parse anywhere else either.
func DefaultFallbackable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	switch llm.StatusCodeOf(err) {
	case 400, 422:
		return false
	default:
		return true
	}
}

func (m *Fallback) HandleRequest(next gateway.RequestHandler) gateway.RequestHandler {
	return func(ctx context.Context, providerName llm.ProviderName, key string, r *llm.Request) (*llm.Response, error) {
		resp, err := next(ctx, providerName, key, r)
		if err == nil || !m.cfg.Fallbackable(err) {
			return resp, err
		}

		for _, target := range m.cfg.Targets {
			targetKey, keyErr := m.keyFor(ctx, target)
			if keyErr != nil {
				err = errors.Join(err, keyErr)
				continue
			}

			resp, targetErr := next(ctx, target.Provider, targetKey, retarget(r, target))
			if targetErr == nil {
				return resp, nil
			}
			err = errors.Join(err, fmt.Errorf("fallback %s: %w", target, targetErr))

			if !m.cfg.Fallbackable(targetErr) {
				break
			}
		}

		return nil, err
	}
}

func (m *Fallback) HandleStreamingRequest(next gateway.StreamingRequestHandler) gateway.StreamingRequestHandler {
	return func(ctx context.Context, providerName llm.ProviderName, key string, r *llm.Request) (*llm.StreamingResponse, error) {
		// target indexes the next candidate, shared between the pre-stream
		// loop below and the in-band one in nextTarget.
		target := 0

		resp, err := next(ctx, providerName, key, r)
		for err != nil && m.cfg.Fallbackable(err) && target < len(m.cfg.Targets) {
			t := m.cfg.Targets[target]
			target++

			targetKey, keyErr := m.keyFor(ctx, t)
			if keyErr != nil {
				err = errors.Join(err, keyErr)
				continue
			}
			resp, err = next(ctx, t.Provider, targetKey, retarget(r, t))
		}
		if err != nil {
			return resp, err
		}

		if resp == nil || resp.ResponsesStreamData == nil {
			return resp, nil
		}

		orig := resp.ResponsesStreamData
		out := make(chan *responses.ResponseChunk)
		resp.ResponsesStreamData = out
		go pumpStream(ctx, out, orig, m.nextTarget(next, r, &target))

		return resp, nil
	}
}

// nextTarget supplies the fallback half of pumpStream: the next target's
// stream, for as long as the chain has one left.
func (m *Fallback) nextTarget(
	next gateway.StreamingRequestHandler,
	r *llm.Request,
	target *int,
) nextStreamFn {
	return func(ctx context.Context, prev *responses.StreamError) (<-chan *responses.ResponseChunk, *responses.ResponseChunk, bool) {
		if !m.cfg.Fallbackable(prev) {
			return nil, nil, false
		}

		for *target < len(m.cfg.Targets) {
			t := m.cfg.Targets[*target]
			*target++

			targetKey, err := m.keyFor(ctx, t)
			if err != nil {
				continue
			}

			resp, err := next(ctx, t.Provider, targetKey, retarget(r, t))
			if err != nil {
				if !m.cfg.Fallbackable(err) {
					return nil, responses.NewStreamError(err), true
				}
				continue
			}
			if resp == nil || resp.ResponsesStreamData == nil {
				continue
			}
			return resp.ResponsesStreamData, nil, true
		}

		return nil, nil, false
	}
}

// keyFor resolves the API key for a target from the config store.
func (m *Fallback) keyFor(ctx context.Context, t FallbackTarget) (string, error) {
	if m.store == nil {
		return "", fmt.Errorf("fallback %s: no config store to resolve an API key from", t)
	}

	cfg, err := m.store.GetProviderConfig(ctx, t.Provider, "")
	if err != nil {
		return "", fmt.Errorf("fallback %s: %w", t, err)
	}

	key := gateway.SelectAPIKey(cfg)
	if key == "" {
		return "", fmt.Errorf("fallback %s: no API key configured", t)
	}
	return key, nil
}

// retarget is r aimed at t, on a copy: the caller still holds the original,
// and every attempt in the chain reissues from the same request.
func retarget(r *llm.Request, t FallbackTarget) *llm.Request {
	out := r.Clone()
	out.SetRequestedModel(t.Model)
	return out
}
