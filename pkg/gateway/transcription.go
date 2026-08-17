package gateway

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/transcription"
)

// Tracing for these requests is handled by TracingMiddleware, not inline.

func (g *LLMGateway) handleTranscriptionRequest(ctx context.Context, providerName llm.ProviderName, p llm.Provider, in *transcription.Request) (*transcription.Response, error) {
	return p.NewTranscription(ctx, in)
}
