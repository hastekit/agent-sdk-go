package mockvectorstore

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/knowledge/vectorstores"
)

// NoOpVectorStore is a placeholder implementation that does nothing.
// Use this when vector store is not configured or during testing.
type NoOpVectorStore struct{}

var _ vectorstores.VectorStore = (*NoOpVectorStore)(nil)

func (n *NoOpVectorStore) EnsureCollection(ctx context.Context, collectionName string, dimension int) error {
	return nil
}

func (n *NoOpVectorStore) UpsertVectors(ctx context.Context, collectionName string, ids []string, embeddings [][]float64, metadata []map[string]any) error {
	return nil
}

func (n *NoOpVectorStore) Search(ctx context.Context, collectionName string, queryVector []float64, limit int, filter map[string]any) ([]vectorstores.SearchResult, error) {
	return nil, nil
}

func (n *NoOpVectorStore) DeleteCollection(ctx context.Context, collectionName string) error {
	return nil
}

func (n *NoOpVectorStore) DeleteVectors(ctx context.Context, collectionName string, ids []string) error {
	return nil
}

func (n *NoOpVectorStore) Close() error {
	return nil
}
