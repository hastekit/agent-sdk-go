package skills

import (
	"context"
	"github.com/hastekit/agent-sdk-go/pkg/cache"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	binary, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skip("redis-server is not installed")
	}
	dir, err := os.MkdirTemp("", "skill-redis-")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	// Unix sockets avoid choosing an available TCP port and racing another test.
	socket := filepath.Join(dir, "redis.sock")
	cmd := exec.Command(binary, "--port", "0", "--unixsocket", socket, "--save", "", "--appendonly", "no")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	client := redis.NewClient(&redis.Options{Network: "unix", Addr: socket})
	t.Cleanup(func() { _ = client.Close() })
	require.Eventually(t, func() bool { return client.Ping(context.Background()).Err() == nil }, 3*time.Second, 20*time.Millisecond)
	return client
}

func TestRedisCacheSharedStoreInvalidation(t *testing.T) {
	client := testRedis(t)
	first, err := cache.NewRedisCache(client, "skills-test:")
	require.NoError(t, err)
	// Separate adapter instances model separate application replicas.
	second, err := cache.NewRedisCache(client, "skills-test:")
	require.NoError(t, err)
	backend := &countingSkillS3{fakeS3: &fakeS3{objects: map[string][]byte{}}}
	a, err := NewS3Store(backend, S3Config{Bucket: "test"}, WithCache(first))
	require.NoError(t, err)
	b, err := NewS3Store(backend, S3Config{Bucket: "test"}, WithCache(second))
	require.NoError(t, err)
	ctx := context.Background()
	_, err = a.Put(ctx, "tenant", bundle("review", "v1"))
	require.NoError(t, err)
	_, err = a.List(ctx, "tenant", ListOptions{})
	require.NoError(t, err)
	page, err := b.List(ctx, "tenant", ListOptions{})
	require.NoError(t, err)
	require.Len(t, page.Skills, 1)
	require.EqualValues(t, 1, backend.lists.Load())
	replacement := bundle("review", "v2")
	replacement.Files["new.md"] = []byte("new")
	_, err = b.Put(ctx, "tenant", replacement)
	require.NoError(t, err)
	page, err = a.List(ctx, "tenant", ListOptions{})
	require.NoError(t, err)
	require.Contains(t, page.Skills[0].Resources, "new.md")
	require.NoError(t, b.Delete(ctx, "tenant", "review"))
	page, err = a.List(ctx, "tenant", ListOptions{})
	require.NoError(t, err)
	require.Empty(t, page.Skills)
	require.EqualValues(t, 3, backend.lists.Load())
}

func TestRedisCacheKVContract(t *testing.T) {
	client := testRedis(t)
	cache, err := cache.NewRedisCache(client, "kv-test:")
	require.NoError(t, err)
	ctx := context.Background()
	_, found, err := cache.Get(ctx, "missing")
	require.NoError(t, err)
	require.False(t, found)
	require.NoError(t, cache.Set(ctx, "key", []byte("value"), time.Minute))
	value, found, err := cache.Get(ctx, "key")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "value", string(value))
	require.Positive(t, client.PTTL(ctx, "kv-test:key").Val())
	require.NoError(t, cache.Delete(ctx, "key"))
	_, found, err = cache.Get(ctx, "key")
	require.NoError(t, err)
	require.False(t, found)
	require.NoError(t, cache.Set(ctx, "key", []byte("value"), 0))
	_, found, err = cache.Get(ctx, "key")
	require.NoError(t, err)
	require.False(t, found)
}
