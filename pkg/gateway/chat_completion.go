package gateway

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/chat_completion"
)

// Tracing for these requests is handled by TracingMiddleware, not inline.

func (g *LLMGateway) handleChatCompletionRequest(ctx context.Context, providerName llm.ProviderName, p llm.Provider, in *chat_completion.Request) (*chat_completion.Response, error) {
	return p.NewChatCompletion(ctx, in)
}

func (g *LLMGateway) handleStreamingChatCompletionRequest(ctx context.Context, providerName llm.ProviderName, p llm.Provider, in *chat_completion.Request) (chan *chat_completion.ResponseChunk, error) {
	return p.NewStreamingChatCompletion(ctx, in)
}
