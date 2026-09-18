package middleware

import (
	"context"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fallback must sit outside retry: a provider gets its full retry budget
// before the chain gives up on it, so a 503 that clears on the second attempt
// never costs a switch to a different model.
func TestFallbackWrapsRetry(t *testing.T) {
	fallback := fallbackWith(twoProviderStore(), FallbackConfig{
		Targets: []FallbackTarget{{Provider: llm.ProviderNameAnthropic, Model: "claude-sonnet-4-5"}},
	})
	retry, _ := testRetry(RetryConfig{MaxAttempts: 3})

	var attempts []attempt
	base := func(_ context.Context, p llm.ProviderName, key string, r *llm.Request) (*llm.Response, error) {
		attempts = append(attempts, attempt{p, key, r.GetRequestedModel()})
		if p == llm.ProviderNameOpenAI {
			return nil, apiErr(503)
		}
		return &llm.Response{}, nil
	}

	handler := fallback.HandleRequest(retry.HandleRequest(base))
	_, err := handler(context.Background(), llm.ProviderNameOpenAI, "openai-key", responsesRequest())

	require.NoError(t, err)
	require.Len(t, attempts, 4)
	for _, a := range attempts[:3] {
		assert.Equal(t, llm.ProviderNameOpenAI, a.provider)
	}
	assert.Equal(t, llm.ProviderNameAnthropic, attempts[3].provider,
		"the chain moves on only after the first provider's budget is spent")
}

// A provider that recovers within its budget must never trigger a fallback.
func TestRetrySuccessPreventsFallback(t *testing.T) {
	fallback := fallbackWith(twoProviderStore(), FallbackConfig{
		Targets: []FallbackTarget{{Provider: llm.ProviderNameAnthropic}},
	})
	retry, _ := testRetry(RetryConfig{MaxAttempts: 3})

	var attempts []attempt
	calls := 0
	base := func(_ context.Context, p llm.ProviderName, key string, r *llm.Request) (*llm.Response, error) {
		attempts = append(attempts, attempt{p, key, r.GetRequestedModel()})
		calls++
		if calls == 1 {
			return nil, apiErr(503)
		}
		return &llm.Response{}, nil
	}

	handler := fallback.HandleRequest(retry.HandleRequest(base))
	_, err := handler(context.Background(), llm.ProviderNameOpenAI, "openai-key", responsesRequest())

	require.NoError(t, err)
	require.Len(t, attempts, 2)
	assert.Equal(t, llm.ProviderNameOpenAI, attempts[1].provider,
		"a transient failure must be absorbed by retry, not escalated to another model")
}
