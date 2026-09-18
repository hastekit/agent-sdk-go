package sdk

import (
	"github.com/hastekit/agent-sdk-go/pkg/gateway"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
)

type ProviderConfig = gateway.ProviderConfig
type APIKeyConfig = gateway.APIKeyConfig

type ProviderName = llm.ProviderName

var (
	ProviderOpenAI     = llm.ProviderNameOpenAI
	ProviderAnthropic  = llm.ProviderNameAnthropic
	ProviderBedrock    = llm.ProviderNameBedrock
	ProviderElevenLabs = llm.ProviderNameElevenLabs
	ProviderGemini     = llm.ProviderNameGemini
	ProviderXAI        = llm.ProviderNameXAI
	ProviderOllama     = llm.ProviderNameOllama
	ProviderOpenRouter = llm.ProviderNameOpenRouter
	ProviderSarvam     = llm.ProviderNameSarvam
	ProviderDeepSeek   = llm.ProviderNameDeepSeek
	ProviderMoonshot   = llm.ProviderNameMoonshot // Kimi models
	ProviderZAI        = llm.ProviderNameZAI      // GLM models
)

// Middleware wraps every call a client makes to a provider — retries,
// fallback, tracing, or anything you write yourself. Build the built-in ones
// with the constructors in pkg/gateway/middleware.
type Middleware = gateway.Middleware

type llmOptions struct {
	// middlewareSet distinguishes a chain that was chosen from one that was
	// never mentioned, which is what lets a model inherit its client's chain
	// while still being able to ask for none.
	middleware    []Middleware
	middlewareSet bool
}

// LLMOption configures the middleware a client, or one model bound off it,
// calls through.
type LLMOption func(*llmOptions)

// WithMiddleware sets the chain, outermost first.
//
//	client := hastekit.NewLLMClient(configs, hastekit.WithMiddleware(
//	    middleware.NewFallbackModels("Anthropic/claude-sonnet-4-5"),
//	    middleware.NewRetry(middleware.RetryConfig{}),
//	))
//
// Order is yours to choose and it matters.
//
// Middleware needing the provider configuration receives it when the chain is
// installed, so nothing here has to be handed a config store.
func WithMiddleware(m ...Middleware) LLMOption {
	return func(o *llmOptions) { o.middleware, o.middlewareSet = m, true }
}

// WithoutMiddleware drops the chain, leaving only tracing. On a model it
// overrides the client's — an LLM judge, say, which should be held to exactly
// what the provider did on the first attempt, under a client that retries.
func WithoutMiddleware() LLMOption {
	return func(o *llmOptions) { o.middleware, o.middlewareSet = nil, true }
}

// newInternalGateway builds a gateway with the middleware chain
func newInternalGateway(configs []ProviderConfig, middlewares []Middleware) *gateway.InternalLLMGateway {
	gw := gateway.NewLLMGateway(gateway.NewInMemoryConfigStore(configs))

	// Copied rather than appended in place: append could otherwise write the
	// tracing entry into the caller's own slice.
	chain := make([]gateway.Middleware, 0, len(middlewares)+1)
	chain = append(chain, middlewares...)
	gw.UseMiddleware(chain...)

	return gateway.NewInternalLLMGateway(gw)
}

type LLMClient struct {
	providerConfigs []ProviderConfig
	llmGateway      *gateway.InternalLLMGateway

	// middleware is the client's chain, kept so a model bound off it can
	// inherit the chain when it does not name one of its own.
	middleware []Middleware
}

// NewLLMClient configures the providers a client can reach, and the
// middleware every model bound off it calls through.
func NewLLMClient(configs []ProviderConfig, opts ...LLMOption) *LLMClient {
	var options llmOptions
	for _, opt := range opts {
		opt(&options)
	}

	return &LLMClient{
		providerConfigs: configs,
		llmGateway:      newInternalGateway(configs, options.middleware),
		middleware:      options.middleware,
	}
}

type Model struct {
	modelId string
	client  llm.Provider
}

// Model binds a "Provider/model" id to a provider the agent can call.
//
// With no options it inherits the client's middleware. WithMiddleware
// replaces that chain wholesale for this one model — there is no merging,
// because a chain is an ordered whole and picking entries out of one by type
// would not survive middleware you wrote yourself:
//
//	fast := client.Model("OpenAI/gpt-4o-mini")
//
//	critical := client.Model("OpenAI/gpt-4o", hastekit.WithMiddleware(
//	    middleware.NewFallbackModels("Anthropic/claude-opus-4-5"),
//	    middleware.NewRetry(middleware.RetryConfig{MaxAttempts: 5}),
//	))
//
//	judge := client.Model("OpenAI/gpt-4o", hastekit.WithoutMiddleware())
//
// A model naming its own chain builds one, so bind a model once at setup
// rather than per request.
func (c *LLMClient) Model(id string, opts ...LLMOption) llm.Provider {
	provider, model := llm.ParseModelID(id)

	gw := c.llmGateway
	if len(opts) > 0 {
		var o llmOptions
		for _, opt := range opts {
			opt(&o)
		}
		if o.middlewareSet {
			gw = newInternalGateway(c.providerConfigs, o.middleware)
		}
	}

	return gateway.NewLLMClient(
		gw,
		gateway.NewInMemoryConfigStore(c.providerConfigs),
		gateway.WithModel(provider, model),
	)
}
