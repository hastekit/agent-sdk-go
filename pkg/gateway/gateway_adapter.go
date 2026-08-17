package gateway

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/chat_completion"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/embeddings"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/image_edit"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/image_generation"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/speech"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/transcription"
)

// InternalLLMGateway uses the internal LLMGatewayAdapter for server-side use.
// This is used within the agent-server where we have direct access to services.
// It handles virtual key resolution and provider configuration from the database.
type InternalLLMGateway struct {
	gateway *LLMGateway
}

// NewInternalLLMGateway creates a provider using the internal gateway.
// The key can be a virtual key (sk-uno-xxx) which will be resolved to actual API keys,
// or a direct API key for the provider.
func NewInternalLLMGateway(gw *LLMGateway) *InternalLLMGateway {
	return &InternalLLMGateway{
		gateway: gw,
	}
}

func (p *InternalLLMGateway) NewResponses(ctx context.Context, providerName llm.ProviderName, key string, req *responses.Request) (*responses.Response, error) {
	llmReq := &llm.Request{
		OfResponsesInput: req,
	}

	resp, err := p.gateway.HandleRequest(ctx, providerName, key, llmReq)
	if err != nil {
		return nil, err
	}

	return resp.OfResponsesOutput, nil
}

func (p *InternalLLMGateway) NewStreamingResponses(ctx context.Context, providerName llm.ProviderName, key string, req *responses.Request) (chan *responses.ResponseChunk, error) {
	llmReq := &llm.Request{
		OfResponsesInput: req,
	}

	streamResp, err := p.gateway.HandleStreamingRequest(ctx, providerName, key, llmReq)
	if err != nil {
		return nil, err
	}

	return streamResp.ResponsesStreamData, nil
}

func (p *InternalLLMGateway) NewEmbedding(ctx context.Context, providerName llm.ProviderName, key string, req *embeddings.Request) (*embeddings.Response, error) {
	llmReq := &llm.Request{
		OfEmbeddingsInput: req,
	}

	resp, err := p.gateway.HandleRequest(ctx, providerName, key, llmReq)
	if err != nil {
		return nil, err
	}

	return resp.OfEmbeddingsOutput, nil
}

func (p *InternalLLMGateway) NewChatCompletion(ctx context.Context, providerName llm.ProviderName, key string, req *chat_completion.Request) (*chat_completion.Response, error) {
	llmReq := &llm.Request{
		OfChatCompletionInput: req,
	}

	resp, err := p.gateway.HandleRequest(ctx, providerName, key, llmReq)
	if err != nil {
		return nil, err
	}

	return resp.OfChatCompletionOutput, nil
}

func (p *InternalLLMGateway) NewStreamingChatCompletion(ctx context.Context, providerName llm.ProviderName, key string, req *chat_completion.Request) (chan *chat_completion.ResponseChunk, error) {
	llmReq := &llm.Request{
		OfChatCompletionInput: req,
	}

	resp, err := p.gateway.HandleStreamingRequest(ctx, providerName, key, llmReq)
	if err != nil {
		return nil, err
	}

	return resp.ChatCompletionStreamData, nil
}

func (p *InternalLLMGateway) NewSpeech(ctx context.Context, providerName llm.ProviderName, key string, req *speech.Request) (*speech.Response, error) {
	llmReq := &llm.Request{
		OfSpeech: req,
	}

	resp, err := p.gateway.HandleRequest(ctx, providerName, key, llmReq)
	if err != nil {
		return nil, err
	}

	return resp.OfSpeech, nil
}

func (p *InternalLLMGateway) NewStreamingSpeech(ctx context.Context, providerName llm.ProviderName, key string, req *speech.Request) (chan *speech.ResponseChunk, error) {
	llmReq := &llm.Request{
		OfSpeech: req,
	}

	resp, err := p.gateway.HandleStreamingRequest(ctx, providerName, key, llmReq)
	if err != nil {
		return nil, err
	}

	return resp.SpeechStreamData, nil
}

func (p *InternalLLMGateway) NewTranscription(ctx context.Context, providerName llm.ProviderName, key string, req *transcription.Request) (*transcription.Response, error) {
	llmReq := &llm.Request{
		OfTranscription: req,
	}

	resp, err := p.gateway.HandleRequest(ctx, providerName, key, llmReq)
	if err != nil {
		return nil, err
	}

	return resp.OfTranscription, nil
}

func (p *InternalLLMGateway) NewImageGeneration(ctx context.Context, providerName llm.ProviderName, key string, req *image_generation.Request) (*image_generation.Response, error) {
	llmReq := &llm.Request{
		OfImageGeneration: req,
	}

	resp, err := p.gateway.HandleRequest(ctx, providerName, key, llmReq)
	if err != nil {
		return nil, err
	}

	return resp.OfImageGeneration, nil
}

func (p *InternalLLMGateway) NewImageEdit(ctx context.Context, providerName llm.ProviderName, key string, req *image_edit.Request) (*image_edit.Response, error) {
	llmReq := &llm.Request{
		OfImageEdit: req,
	}

	resp, err := p.gateway.HandleRequest(ctx, providerName, key, llmReq)
	if err != nil {
		return nil, err
	}

	return resp.OfImageEdit, nil
}
