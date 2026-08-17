package embedder

import (
	"context"
	"fmt"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/embeddings"
)

// GatewayEmbedder implements Embedder by calling the LLM gateway.
// Used for semantic chunking which needs per-sentence embeddings.
type GatewayEmbedder struct {
	provider  llm.Provider
	dimension int
}

// NewGatewayEmbedder returns an embedder that uses the LLM gateway with the knowledge's
// embedding provider/model and the project's default key. Requires ctx to load the project.
func NewGatewayEmbedder(provider llm.Provider, dim int) Embedder {
	return &GatewayEmbedder{
		provider:  provider,
		dimension: dim,
	}
}

func (e *GatewayEmbedder) Embed(ctx context.Context, text string) ([]float64, error) {
	req := &embeddings.Request{
		Dimensions: &e.dimension,
		Input:      embeddings.InputUnion{OfString: &text},
	}

	resp, err := e.provider.NewEmbedding(ctx, req)
	if err != nil {
		return nil, err
	}

	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("no embedding in response")
	}

	of := resp.Data[0].Embedding.OfFloat
	if of == nil {
		return nil, fmt.Errorf("embedding data not float")
	}

	return of, nil
}

func (e *GatewayEmbedder) EmbedBatch(ctx context.Context, text []string) ([][]float64, error) {
	req := &embeddings.Request{
		Dimensions: &e.dimension,
		Input:      embeddings.InputUnion{OfList: text},
	}

	resp, err := e.provider.NewEmbedding(ctx, req)
	if err != nil {
		return nil, err
	}

	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("no embedding in response")
	}

	result := make([][]float64, len(resp.Data))
	for idx, d := range resp.Data {
		if d.Embedding.OfFloat != nil {
			result[idx] = d.Embedding.OfFloat
		} else {
			result[idx] = nil
		}
	}

	return result, nil
}

var _ Embedder = (*GatewayEmbedder)(nil)
