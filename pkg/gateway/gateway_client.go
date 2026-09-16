package gateway

import (
	"context"
	"errors"
	"strings"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/chat_completion"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/embeddings"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/image_edit"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/image_generation"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/speech"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/transcription"
	utils2 "github.com/hastekit/agent-sdk-go/pkg/utils"
)

// LLMGatewayAdapter is the interface for making LLM calls.
// Similar to ConversationPersistenceAdapter, it can be implemented by:
// - InternalLLMProvider: uses the internal gateway (for server-side)
// - ExternalLLMProvider: calls agent-server via HTTP (for SDK consumers)
type LLMGatewayAdapter interface {
	// NewResponses makes a non-streaming LLM call
	NewResponses(ctx context.Context, provider llm.ProviderName, key string, req *responses.Request) (*responses.Response, error)

	// NewStreamingResponses makes a streaming LLM call
	NewStreamingResponses(ctx context.Context, provider llm.ProviderName, key string, req *responses.Request) (chan *responses.ResponseChunk, error)

	// NewEmbedding
	NewEmbedding(ctx context.Context, providerName llm.ProviderName, key string, req *embeddings.Request) (*embeddings.Response, error)

	// NewChatCompletion
	NewChatCompletion(ctx context.Context, providerName llm.ProviderName, key string, req *chat_completion.Request) (*chat_completion.Response, error)

	// NewStreamingChatCompletion
	NewStreamingChatCompletion(ctx context.Context, providerName llm.ProviderName, key string, req *chat_completion.Request) (chan *chat_completion.ResponseChunk, error)

	// NewSpeech
	NewSpeech(ctx context.Context, providerName llm.ProviderName, key string, req *speech.Request) (*speech.Response, error)

	// NewStreamingSpeech
	NewStreamingSpeech(ctx context.Context, providerName llm.ProviderName, key string, req *speech.Request) (chan *speech.ResponseChunk, error)

	// NewTranscription
	NewTranscription(ctx context.Context, providerName llm.ProviderName, key string, req *transcription.Request) (*transcription.Response, error)

	// NewImageGeneration
	NewImageGeneration(ctx context.Context, providerName llm.ProviderName, key string, req *image_generation.Request) (*image_generation.Response, error)

	// NewImageEdit
	NewImageEdit(ctx context.Context, providerName llm.ProviderName, key string, req *image_edit.Request) (*image_edit.Response, error)
}

// LLMClient wraps an LLMGatewayAdapter and provides a high-level interface
type LLMClient struct {
	LLMGatewayAdapter
	configStore ConfigStore

	provider llm.ProviderName
	key      string
	model    string
}

type LLMClientOption func(*LLMClient)

func WithKey(key string) LLMClientOption {
	return func(c *LLMClient) {
		c.key = key
	}
}

func WithModel(providerName llm.ProviderName, model string) LLMClientOption {
	return func(c *LLMClient) {
		c.provider = providerName
		c.model = model
	}
}

// NewLLMClient creates a new LLM client with the given provider.
func NewLLMClient(p LLMGatewayAdapter, configStore ConfigStore, opts ...LLMClientOption) *LLMClient {
	cli := &LLMClient{
		LLMGatewayAdapter: p,
		configStore:       configStore,
	}

	for _, opt := range opts {
		opt(cli)
	}

	return cli
}

func (c *LLMClient) NewResponses(ctx context.Context, in *responses.Request) (*responses.Response, error) {
	providerName, model, err := c.getProviderAndModelName(in.Model)
	if err != nil {
		return nil, err
	}
	in.Model = model

	in.Stream = utils2.Ptr(false)
	in.Store = utils2.Ptr(false)
	return c.LLMGatewayAdapter.NewResponses(ctx, providerName, c.getKey(ctx, providerName), in)
}

// NewStreamingResponses invokes the LLM and streams responses via callback
func (c *LLMClient) NewStreamingResponses(ctx context.Context, in *responses.Request) (chan *responses.ResponseChunk, error) {
	providerName, model, err := c.getProviderAndModelName(in.Model)
	if err != nil {
		return nil, err
	}
	in.Model = model

	in.Stream = utils2.Ptr(true)
	in.Store = utils2.Ptr(false)
	return c.LLMGatewayAdapter.NewStreamingResponses(ctx, providerName, c.getKey(ctx, providerName), in)
}

// NewStreamingResponsesForModel explicitly routes one call through this client's
// adapter and config store. A key bound to a different provider is never reused.
func (c *LLMClient) NewStreamingResponsesForModel(ctx context.Context, target string, in *responses.Request) (chan *responses.ResponseChunk, error) {
	parts := strings.SplitN(target, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, errors.New("model target must be Provider/model")
	}
	provider := llm.ProviderName(parts[0])
	key := c.key
	if provider != c.provider {
		key = ""
	}
	if key == "" {
		if c.configStore == nil {
			return nil, errors.New("no provider config store for model target")
		}
		cfg, err := c.configStore.GetProviderConfig(ctx, provider, ProviderConfigKeyFromContext(ctx))
		if err != nil {
			return nil, err
		}
		key = SelectAPIKey(cfg)
	}
	prepared := *in
	prepared.Model = parts[1]
	prepared.Stream = utils2.Ptr(true)
	prepared.Store = utils2.Ptr(false)
	return c.LLMGatewayAdapter.NewStreamingResponses(ctx, provider, key, &prepared)
}

func (c *LLMClient) NewEmbedding(ctx context.Context, in *embeddings.Request) (*embeddings.Response, error) {
	providerName, model, err := c.getProviderAndModelName(in.Model)
	if err != nil {
		return nil, err
	}
	in.Model = model

	return c.LLMGatewayAdapter.NewEmbedding(ctx, providerName, c.getKey(ctx, providerName), in)
}

func (c *LLMClient) NewChatCompletion(ctx context.Context, in *chat_completion.Request) (*chat_completion.Response, error) {
	providerName, model, err := c.getProviderAndModelName(in.Model)
	if err != nil {
		return nil, err
	}
	in.Model = model

	return c.LLMGatewayAdapter.NewChatCompletion(ctx, providerName, c.getKey(ctx, providerName), in)
}

func (c *LLMClient) NewStreamingChatCompletion(ctx context.Context, in *chat_completion.Request) (chan *chat_completion.ResponseChunk, error) {
	providerName, model, err := c.getProviderAndModelName(in.Model)
	if err != nil {
		return nil, err
	}
	in.Model = model

	return c.LLMGatewayAdapter.NewStreamingChatCompletion(ctx, providerName, c.getKey(ctx, providerName), in)
}

func (c *LLMClient) NewSpeech(ctx context.Context, in *speech.Request) (*speech.Response, error) {
	providerName, model, err := c.getProviderAndModelName(in.Model)
	if err != nil {
		return nil, err
	}
	in.Model = model

	return c.LLMGatewayAdapter.NewSpeech(ctx, providerName, c.getKey(ctx, providerName), in)
}

func (c *LLMClient) NewStreamingSpeech(ctx context.Context, in *speech.Request) (chan *speech.ResponseChunk, error) {
	providerName, model, err := c.getProviderAndModelName(in.Model)
	if err != nil {
		return nil, err
	}
	in.Model = model

	return c.LLMGatewayAdapter.NewStreamingSpeech(ctx, providerName, c.getKey(ctx, providerName), in)
}

func (c *LLMClient) NewTranscription(ctx context.Context, in *transcription.Request) (*transcription.Response, error) {
	providerName, model, err := c.getProviderAndModelName(in.Model)
	if err != nil {
		return nil, err
	}
	in.Model = model

	return c.LLMGatewayAdapter.NewTranscription(ctx, providerName, c.getKey(ctx, providerName), in)
}

func (c *LLMClient) NewImageGeneration(ctx context.Context, in *image_generation.Request) (*image_generation.Response, error) {
	providerName, model, err := c.getProviderAndModelName(in.Model)
	if err != nil {
		return nil, err
	}
	in.Model = model

	return c.LLMGatewayAdapter.NewImageGeneration(ctx, providerName, c.getKey(ctx, providerName), in)
}

func (c *LLMClient) NewImageEdit(ctx context.Context, in *image_edit.Request) (*image_edit.Response, error) {
	providerName, model, err := c.getProviderAndModelName(in.Model)
	if err != nil {
		return nil, err
	}
	in.Model = model

	return c.LLMGatewayAdapter.NewImageEdit(ctx, providerName, c.getKey(ctx, providerName), in)
}

func (c *LLMClient) getKey(ctx context.Context, providerName llm.ProviderName) string {
	if c.key != "" {
		return c.key
	}

	if c.configStore == nil {
		return ""
	}

	providerConfig, err := c.configStore.GetProviderConfig(ctx, providerName, ProviderConfigKeyFromContext(ctx))
	if err != nil {
		return ""
	}

	return SelectAPIKey(providerConfig)
}

// SelectAPIKey picks one of a provider's configured keys, weighted by
// APIKeyConfig.Weight. It is exported for middleware.Fallback, which has to
// resolve a key for a provider the caller never named.
func SelectAPIKey(cfg *ProviderConfig) string {
	if cfg == nil || len(cfg.ApiKeys) == 0 {
		return ""
	}

	if len(cfg.ApiKeys) == 1 {
		return cfg.ApiKeys[0].APIKey
	}

	// Weight random selection
	weights := make([]int, len(cfg.ApiKeys))
	for idx, key := range cfg.ApiKeys {
		weights[idx] = key.Weight
	}

	return cfg.ApiKeys[utils2.WeightedRandomIndex(weights)].APIKey
}

func (c *LLMClient) getProviderAndModelName(input string) (llm.ProviderName, string, error) {
	if c.provider != "" {
		return c.provider, c.model, nil
	}

	frag := strings.SplitN(input, "/", 2)
	if len(frag) != 2 {
		return "", "", errors.New("invalid input")
	}

	return llm.ProviderName(frag[0]), frag[1], nil
}
