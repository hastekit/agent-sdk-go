package middleware

import (
	"context"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/gateway"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// attempt is one call the chain made, as the fake handler saw it.
type attempt struct {
	provider llm.ProviderName
	key      string
	model    string
}

// fallbackWith is NewFallback plus the store the gateway would normally
// inject, for tests that drive the middleware without installing it.
func fallbackWith(store gateway.ConfigStore, cfg FallbackConfig) gateway.Middleware {
	return NewFallback(cfg).WithConfigStore(store)
}

func twoProviderStore() gateway.ConfigStore {
	return gateway.NewInMemoryConfigStore([]gateway.ProviderConfig{
		{
			ProviderName: llm.ProviderNameOpenAI,
			ApiKeys:      []*gateway.APIKeyConfig{{Name: "default", APIKey: "openai-key"}},
		},
		{
			ProviderName: llm.ProviderNameAnthropic,
			ApiKeys:      []*gateway.APIKeyConfig{{Name: "default", APIKey: "anthropic-key"}},
		},
	})
}

func TestFallbackMovesToTheNextProvider(t *testing.T) {
	m := fallbackWith(twoProviderStore(), FallbackConfig{
		Targets: []FallbackTarget{{Provider: llm.ProviderNameAnthropic, Model: "claude-sonnet-4-5"}},
	})

	var attempts []attempt
	handler := m.HandleRequest(func(_ context.Context, p llm.ProviderName, key string, r *llm.Request) (*llm.Response, error) {
		attempts = append(attempts, attempt{p, key, r.GetRequestedModel()})
		if p == llm.ProviderNameOpenAI {
			return nil, apiErr(503)
		}
		return &llm.Response{}, nil
	})

	resp, err := handler(context.Background(), llm.ProviderNameOpenAI, "openai-key", responsesRequest())

	require.NoError(t, err)
	assert.NotNil(t, resp)
	require.Len(t, attempts, 2)
	assert.Equal(t, attempt{llm.ProviderNameOpenAI, "openai-key", "gpt-4o-mini"}, attempts[0])
	assert.Equal(t, attempt{llm.ProviderNameAnthropic, "anthropic-key", "claude-sonnet-4-5"}, attempts[1],
		"the target's own key must be resolved from the config store")
}

func TestFallbackDoesNotMutateTheCallersRequest(t *testing.T) {
	m := fallbackWith(twoProviderStore(), FallbackConfig{
		Targets: []FallbackTarget{{Provider: llm.ProviderNameAnthropic, Model: "claude-sonnet-4-5"}},
	})

	handler := m.HandleRequest(func(_ context.Context, p llm.ProviderName, _ string, _ *llm.Request) (*llm.Response, error) {
		if p == llm.ProviderNameOpenAI {
			return nil, apiErr(503)
		}
		return &llm.Response{}, nil
	})

	req := responsesRequest()
	_, err := handler(context.Background(), llm.ProviderNameOpenAI, "openai-key", req)

	require.NoError(t, err)
	assert.Equal(t, "gpt-4o-mini", req.OfResponsesInput.Model,
		"the caller still holds this request; the fallback model must not leak into it")
}

func TestFallbackKeepsTheModelWhenTheTargetNamesNone(t *testing.T) {
	store := gateway.NewInMemoryConfigStore([]gateway.ProviderConfig{
		{ProviderName: llm.ProviderNameOpenAI, ApiKeys: []*gateway.APIKeyConfig{{APIKey: "openai-key"}}},
		{ProviderName: llm.ProviderNameOpenRouter, ApiKeys: []*gateway.APIKeyConfig{{APIKey: "or-key"}}},
	})
	m := fallbackWith(store, FallbackConfig{
		Targets: []FallbackTarget{{Provider: llm.ProviderNameOpenRouter}},
	})

	var attempts []attempt
	handler := m.HandleRequest(func(_ context.Context, p llm.ProviderName, key string, r *llm.Request) (*llm.Response, error) {
		attempts = append(attempts, attempt{p, key, r.GetRequestedModel()})
		if p == llm.ProviderNameOpenAI {
			return nil, apiErr(503)
		}
		return &llm.Response{}, nil
	})

	_, err := handler(context.Background(), llm.ProviderNameOpenAI, "openai-key", responsesRequest())

	require.NoError(t, err)
	require.Len(t, attempts, 2)
	assert.Equal(t, "gpt-4o-mini", attempts[1].model)
}

func TestFallbackLeavesMalformedRequestsAlone(t *testing.T) {
	m := fallbackWith(twoProviderStore(), FallbackConfig{
		Targets: []FallbackTarget{{Provider: llm.ProviderNameAnthropic}},
	})

	calls := 0
	handler := m.HandleRequest(func(context.Context, llm.ProviderName, string, *llm.Request) (*llm.Response, error) {
		calls++
		return nil, apiErr(400)
	})

	_, err := handler(context.Background(), llm.ProviderNameOpenAI, "openai-key", responsesRequest())

	require.Error(t, err)
	assert.Equal(t, 1, calls, "a request no provider can parse must not be sprayed across the chain")
}

func TestFallbackReportsEveryFailureWhenTheChainRunsOut(t *testing.T) {
	m := fallbackWith(twoProviderStore(), FallbackConfig{
		Targets: []FallbackTarget{{Provider: llm.ProviderNameAnthropic, Model: "claude-sonnet-4-5"}},
	})

	handler := m.HandleRequest(func(context.Context, llm.ProviderName, string, *llm.Request) (*llm.Response, error) {
		return nil, apiErr(503)
	})

	_, err := handler(context.Background(), llm.ProviderNameOpenAI, "openai-key", responsesRequest())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "Anthropic/claude-sonnet-4-5",
		"the joined error should name the target that was tried")
}

func TestFallbackSkipsATargetItCannotKey(t *testing.T) {
	store := gateway.NewInMemoryConfigStore([]gateway.ProviderConfig{
		{ProviderName: llm.ProviderNameOpenAI, ApiKeys: []*gateway.APIKeyConfig{{APIKey: "openai-key"}}},
		{ProviderName: llm.ProviderNameAnthropic, ApiKeys: []*gateway.APIKeyConfig{{APIKey: "anthropic-key"}}},
	})
	m := fallbackWith(store, FallbackConfig{
		Targets: []FallbackTarget{
			{Provider: llm.ProviderNameGemini}, // never configured
			{Provider: llm.ProviderNameAnthropic},
		},
	})

	var attempts []attempt
	handler := m.HandleRequest(func(_ context.Context, p llm.ProviderName, key string, r *llm.Request) (*llm.Response, error) {
		attempts = append(attempts, attempt{p, key, r.GetRequestedModel()})
		if p == llm.ProviderNameOpenAI {
			return nil, apiErr(503)
		}
		return &llm.Response{}, nil
	})

	_, err := handler(context.Background(), llm.ProviderNameOpenAI, "openai-key", responsesRequest())

	require.NoError(t, err)
	require.Len(t, attempts, 2, "the unkeyed target is skipped, not called")
	assert.Equal(t, llm.ProviderNameAnthropic, attempts[1].provider)
}

func TestFallbackWithoutTargetsIsANoOp(t *testing.T) {
	m := fallbackWith(twoProviderStore(), FallbackConfig{})

	calls := 0
	handler := m.HandleRequest(func(context.Context, llm.ProviderName, string, *llm.Request) (*llm.Response, error) {
		calls++
		return nil, apiErr(503)
	})

	_, err := handler(context.Background(), llm.ProviderNameOpenAI, "openai-key", responsesRequest())

	require.Error(t, err)
	assert.Equal(t, 1, calls)
}

// --- streaming ------------------------------------------------------------

func TestFallbackStreamMovesOnBeforeTheStreamOpens(t *testing.T) {
	m := fallbackWith(twoProviderStore(), FallbackConfig{
		Targets: []FallbackTarget{{Provider: llm.ProviderNameAnthropic, Model: "claude-sonnet-4-5"}},
	})

	var attempts []attempt
	handler := m.HandleStreamingRequest(func(_ context.Context, p llm.ProviderName, key string, r *llm.Request) (*llm.StreamingResponse, error) {
		attempts = append(attempts, attempt{p, key, r.GetRequestedModel()})
		if p == llm.ProviderNameOpenAI {
			return nil, apiErr(503)
		}
		ch, _ := stream(textChunk("from claude"))
		return &llm.StreamingResponse{ResponsesStreamData: ch}, nil
	})

	resp, err := handler(context.Background(), llm.ProviderNameOpenAI, "openai-key", responsesRequest())
	require.NoError(t, err)

	got := collect(t, resp.ResponsesStreamData)
	require.Len(t, got, 1)
	assert.Equal(t, "from claude", got[0].OfOutputTextDelta.Delta)
	require.Len(t, attempts, 2)
	assert.Equal(t, "claude-sonnet-4-5", attempts[1].model)
}

func TestFallbackStreamMovesOnAfterAnErrorInTheFirstChunk(t *testing.T) {
	m := fallbackWith(twoProviderStore(), FallbackConfig{
		Targets: []FallbackTarget{{Provider: llm.ProviderNameAnthropic, Model: "claude-sonnet-4-5"}},
	})

	var firstDone chan struct{}
	handler := m.HandleStreamingRequest(func(_ context.Context, p llm.ProviderName, _ string, _ *llm.Request) (*llm.StreamingResponse, error) {
		if p == llm.ProviderNameOpenAI {
			ch, done := stream(errorChunk("overloaded"))
			firstDone = done
			return &llm.StreamingResponse{ResponsesStreamData: ch}, nil
		}
		ch, _ := stream(textChunk("from claude"))
		return &llm.StreamingResponse{ResponsesStreamData: ch}, nil
	})

	resp, err := handler(context.Background(), llm.ProviderNameOpenAI, "openai-key", responsesRequest())
	require.NoError(t, err)

	got := collect(t, resp.ResponsesStreamData)
	require.Len(t, got, 1)
	assert.Equal(t, "from claude", got[0].OfOutputTextDelta.Delta)

	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the abandoned attempt was not drained")
	}
}

func TestFallbackStreamCommitsOnceAChunkIsDelivered(t *testing.T) {
	m := fallbackWith(twoProviderStore(), FallbackConfig{
		Targets: []FallbackTarget{{Provider: llm.ProviderNameAnthropic}},
	})

	calls := 0
	handler := m.HandleStreamingRequest(func(context.Context, llm.ProviderName, string, *llm.Request) (*llm.StreamingResponse, error) {
		calls++
		ch, _ := stream(textChunk("half an"), errorChunk("died mid-flight"))
		return &llm.StreamingResponse{ResponsesStreamData: ch}, nil
	})

	resp, err := handler(context.Background(), llm.ProviderNameOpenAI, "openai-key", responsesRequest())
	require.NoError(t, err)

	got := collect(t, resp.ResponsesStreamData)
	require.Len(t, got, 2)
	assert.Equal(t, 1, calls, "half an answer from one model must not be finished by another")
}

func TestFallbackStreamReportsTheLastFailureWhenTheChainRunsOut(t *testing.T) {
	m := fallbackWith(twoProviderStore(), FallbackConfig{
		Targets: []FallbackTarget{{Provider: llm.ProviderNameAnthropic}},
	})

	handler := m.HandleStreamingRequest(func(context.Context, llm.ProviderName, string, *llm.Request) (*llm.StreamingResponse, error) {
		ch, _ := stream(errorChunk("overloaded"))
		return &llm.StreamingResponse{ResponsesStreamData: ch}, nil
	})

	resp, err := handler(context.Background(), llm.ProviderNameOpenAI, "openai-key", responsesRequest())
	require.NoError(t, err)

	got := collect(t, resp.ResponsesStreamData)
	require.Len(t, got, 1)
	require.NotNil(t, got[0].OfError)
}

// --- store injection ------------------------------------------------------

func TestFallbackWithoutAStoreCannotResolveATarget(t *testing.T) {
	m := NewFallback(FallbackConfig{
		Targets: []FallbackTarget{{Provider: llm.ProviderNameAnthropic}},
	})

	calls := 0
	handler := m.HandleRequest(func(context.Context, llm.ProviderName, string, *llm.Request) (*llm.Response, error) {
		calls++
		return nil, apiErr(503)
	})

	_, err := handler(context.Background(), llm.ProviderNameOpenAI, "openai-key", responsesRequest())

	require.Error(t, err)
	assert.Equal(t, 1, calls, "an unresolvable target is skipped, not called")
	assert.Contains(t, err.Error(), "no config store")
}

func TestWithConfigStoreDoesNotOverwriteAnExplicitStore(t *testing.T) {
	explicit := twoProviderStore()
	m := NewFallback(FallbackConfig{}).WithConfigStore(explicit)

	// A second injection, as UseMiddleware would do, must leave the first.
	again := m.(*Fallback).WithConfigStore(nil)

	assert.Same(t, explicit, again.(*Fallback).store, "a store set by the caller wins")
}

func TestWithConfigStoreCopiesRatherThanMutates(t *testing.T) {
	original := NewFallback(FallbackConfig{})
	bound := original.WithConfigStore(twoProviderStore())

	assert.Nil(t, original.store, "middleware shared between gateways must not be re-pointed")
	assert.NotNil(t, bound.(*Fallback).store)
}

func TestFallbackModelsParsesIDs(t *testing.T) {
	targets := FallbackModels("Anthropic/claude-sonnet-4-5", "OpenRouter")

	require.Len(t, targets, 2)
	assert.Equal(t, FallbackTarget{Provider: llm.ProviderNameAnthropic, Model: "claude-sonnet-4-5"}, targets[0])
	assert.Equal(t, FallbackTarget{Provider: llm.ProviderNameOpenRouter}, targets[1],
		"an id naming only a provider keeps the model the caller asked for")
}
