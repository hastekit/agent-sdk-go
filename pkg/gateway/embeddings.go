package gateway

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/embeddings"
)

// Tracing for these requests is handled by TracingMiddleware, not inline.

func (g *LLMGateway) handleEmbeddingsRequest(ctx context.Context, providerName llm.ProviderName, p llm.Provider, in *embeddings.Request) (*embeddings.Response, error) {
	return p.NewEmbedding(ctx, in)
}
