package gateway

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/speech"
)

// Tracing for these requests is handled by TracingMiddleware, not inline.

func (g *LLMGateway) handleSpeechRequest(ctx context.Context, providerName llm.ProviderName, p llm.Provider, in *speech.Request) (*speech.Response, error) {
	return p.NewSpeech(ctx, in)
}

func (g *LLMGateway) handleStreamingSpeechRequest(ctx context.Context, providerName llm.ProviderName, p llm.Provider, in *speech.Request) (chan *speech.ResponseChunk, error) {
	return p.NewStreamingSpeech(ctx, in)
}
