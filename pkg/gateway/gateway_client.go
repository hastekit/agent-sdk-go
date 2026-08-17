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

	if len(providerConfig.ApiKeys) == 0 {
		return ""
	}

	if len(providerConfig.ApiKeys) == 1 {
		return providerConfig.ApiKeys[0].APIKey
	}

	// Weight random selection
	weights := make([]int, len(providerConfig.ApiKeys))
	for idx, key := range providerConfig.ApiKeys {
		weights[idx] = key.Weight
	}

	return providerConfig.ApiKeys[utils2.WeightedRandomIndex(weights)].APIKey
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
