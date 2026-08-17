package gateway

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/image_generation"
)

// Tracing for these requests is handled by TracingMiddleware, not inline.

func (g *LLMGateway) handleImageGenerationRequest(ctx context.Context, providerName llm.ProviderName, p llm.Provider, in *image_generation.Request) (*image_generation.Response, error) {
	return p.NewImageGeneration(ctx, in)
}
