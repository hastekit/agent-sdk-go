package streambroker

import (
	"context"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// Replay returns a snapshot of the retained chunks, in publication order.
// It does not subscribe or alter the stream. Chunks must be treated as immutable.
func (b *MemoryStreamBroker) Replay(ctx context.Context, channel string) ([]*responses.ResponseChunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]*responses.ResponseChunk(nil), b.transcripts[channel]...), nil
}

// Replay returns the currently retained Redis stream, excluding its close marker.
func (b *RedisStreamBroker) Replay(ctx context.Context, channel string) ([]*responses.ResponseChunk, error) {
	started := time.Now()
	entries, err := b.client.XRange(ctx, b.streamKey(channel), "-", "+").Result()
	b.observe(ctx, "replay_snapshot", channel, started, 0, err)
	if err != nil {
		return nil, err
	}
	chunks := make([]*responses.ResponseChunk, 0, len(entries))
	for _, entry := range entries {
		chunk, end, ok := decodeEntry(entry)
		if end {
			break
		}
		if ok {
			chunks = append(chunks, chunk)
		}
	}
	return chunks, nil
}
