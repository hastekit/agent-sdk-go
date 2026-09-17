package cache

import (
	"context"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestMemoryCacheTTLAndCopies(t *testing.T) {
	ctx := context.Background()
	kv := NewMemoryCache()
	input := []byte("value")
	require.NoError(t, kv.Set(ctx, "key", input, time.Minute))
	input[0] = 'x'
	data, found, err := kv.Get(ctx, "key")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "value", string(data))
	data[0] = 'x'
	data, _, _ = kv.Get(ctx, "key")
	require.Equal(t, "value", string(data))
	kv.mu.Lock()
	entry := kv.entries["key"]
	entry.expires = time.Now().Add(-time.Second)
	kv.entries["key"] = entry
	kv.mu.Unlock()
	_, found, err = kv.Get(ctx, "key")
	require.NoError(t, err)
	require.False(t, found)
	for i := 0; i < 300; i++ {
		require.NoError(t, kv.Set(ctx, time.Unix(int64(i), 0).String(), input, time.Minute))
	}
	require.Len(t, kv.entries, 256)
}
