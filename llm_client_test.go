package sdk

import (
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/gateway"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testConfigs() []ProviderConfig {
	return []ProviderConfig{
		{
			ProviderName: ProviderOpenAI,
			ApiKeys:      []*APIKeyConfig{{Name: "default", APIKey: "openai-key"}},
		},
		{
			ProviderName: ProviderAnthropic,
			ApiKeys:      []*APIKeyConfig{{Name: "default", APIKey: "anthropic-key"}},
		},
	}
}

// marker is a custom middleware, standing in for whatever a user writes.
type marker struct{}

func (marker) HandleRequest(next gateway.RequestHandler) gateway.RequestHandler { return next }
func (marker) HandleStreamingRequest(next gateway.StreamingRequestHandler) gateway.StreamingRequestHandler {
	return next
}

// gatewayOf reaches for the adapter a bound model calls through, which is how
// these tests tell an inherited chain from a model's own.
func gatewayOf(t *testing.T, p llm.Provider) gateway.LLMGatewayAdapter {
	t.Helper()
	client, ok := p.(*gateway.LLMClient)
	require.True(t, ok, "Model should bind a *gateway.LLMClient")
	return client.LLMGatewayAdapter
}

// --- client ---------------------------------------------------------------

func TestClientInstallsNothingByDefault(t *testing.T) {
	client := NewLLMClient(testConfigs())

	assert.Empty(t, client.middleware, "middleware must be asked for")
}

func TestClientKeepsTheChainItWasGiven(t *testing.T) {
	retry := middleware.NewRetry(middleware.RetryConfig{})
	fallback := middleware.NewFallbackModels("Anthropic/claude-sonnet-4-5")

	client := NewLLMClient(testConfigs(), WithMiddleware(fallback, retry))

	require.Len(t, client.middleware, 2)
	assert.Same(t, fallback, client.middleware[0], "order is the caller's, outermost first")
	assert.Same(t, retry, client.middleware[1])
}

func TestClientAcceptsCustomMiddleware(t *testing.T) {
	client := NewLLMClient(testConfigs(), WithMiddleware(marker{}))

	require.Len(t, client.middleware, 1)
	assert.IsType(t, marker{}, client.middleware[0])
}

// Tracing is added by the client rather than asked for, so a caller supplying
// their own chain never loses it.
func TestClientAppendsTracingWithoutTouchingTheCallersSlice(t *testing.T) {
	chain := []Middleware{marker{}}

	NewLLMClient(testConfigs(), WithMiddleware(chain...))

	require.Len(t, chain, 1, "appending tracing must not write into the caller's slice")
}

// --- model scope ----------------------------------------------------------

func TestModelWithoutOptionsInheritsTheClientChain(t *testing.T) {
	client := NewLLMClient(testConfigs(), WithMiddleware(middleware.NewRetry(middleware.RetryConfig{})))

	model := client.Model("OpenAI/gpt-4o-mini")

	assert.Same(t, client.llmGateway, gatewayOf(t, model),
		"a model that asks for nothing should reuse the client's chain rather than rebuild it")
}

func TestModelWithMiddlewareGetsItsOwnChain(t *testing.T) {
	client := NewLLMClient(testConfigs())

	model := client.Model("OpenAI/gpt-4o-mini", WithMiddleware(marker{}))

	assert.NotSame(t, client.llmGateway, gatewayOf(t, model))
}

func TestModelWithoutMiddlewareGetsItsOwnChain(t *testing.T) {
	client := NewLLMClient(testConfigs(), WithMiddleware(middleware.NewRetry(middleware.RetryConfig{})))

	model := client.Model("OpenAI/gpt-4o-mini", WithoutMiddleware())

	assert.NotSame(t, client.llmGateway, gatewayOf(t, model),
		"a model dropping the chain must not keep calling through the client's")
}

func TestModelBindsTheProviderAndModel(t *testing.T) {
	client := NewLLMClient(testConfigs())

	bound, ok := client.Model("Anthropic/claude-sonnet-4-5", WithMiddleware(marker{})).(*gateway.LLMClient)

	require.True(t, ok)
	assert.NotNil(t, bound, "options must not cost the model its binding")
}

// --- model ids ------------------------------------------------------------

func TestModelIDParsing(t *testing.T) {
	cases := []struct {
		id       string
		provider ProviderName
		model    string
	}{
		{"OpenAI/gpt-4o-mini", ProviderOpenAI, "gpt-4o-mini"},
		{"Bedrock/us.anthropic.claude-sonnet-4-5-v1:0", ProviderBedrock, "us.anthropic.claude-sonnet-4-5-v1:0"},
		{"OpenRouter", ProviderOpenRouter, ""},
	}

	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			provider, model := llm.ParseModelID(tc.id)
			assert.Equal(t, tc.provider, provider)
			assert.Equal(t, tc.model, model)
		})
	}
}
