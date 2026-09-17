package skills

import (
	"context"
	"errors"
	"github.com/hastekit/agent-sdk-go/pkg/cache"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/require"
)

func TestListCacheIsolationAndCopies(t *testing.T) {
	kv := cache.NewMemoryCache()
	c := listCache{kv: kv, ttl: time.Minute}
	calls := 0
	load := func() (Page, error) {
		calls++
		return Page{Skills: []Metadata{{Name: "review", Resources: []string{"ref.md"}}}, NextCursor: "next"}, nil
	}
	ctx := context.Background()
	page, err := c.list(ctx, "a", ListOptions{}, load)
	require.NoError(t, err)
	page.Skills[0].Name = "changed"
	page.Skills[0].Resources[0] = "changed"
	page, err = c.list(ctx, "a", ListOptions{Limit: 50}, load)
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.Equal(t, "review", page.Skills[0].Name)
	require.Equal(t, []string{"ref.md"}, page.Skills[0].Resources)
	for _, opts := range []ListOptions{{Limit: 1}, {Cursor: "next"}} {
		_, err = c.list(ctx, "a", opts, load)
		require.NoError(t, err)
	}
	_, err = c.list(ctx, "b", ListOptions{}, load)
	require.NoError(t, err)
	require.Equal(t, 4, calls)
	require.NoError(t, c.invalidate(ctx, "a"))
	_, err = c.list(ctx, "b", ListOptions{}, load)
	require.NoError(t, err)
	require.Equal(t, 4, calls)
	_, err = c.list(ctx, "a", ListOptions{}, load)
	require.NoError(t, err)
	require.Equal(t, 5, calls)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = c.list(canceled, "a", ListOptions{}, load)
	require.ErrorIs(t, err, context.Canceled)
	// Eviction of a generation marker must not make older pages reachable.
	require.NoError(t, kv.Delete(ctx, versionKey("a")))
	_, err = c.list(ctx, "a", ListOptions{}, load)
	require.NoError(t, err)
	require.Equal(t, 6, calls)
}

func TestListCacheInvalidationDuringRead(t *testing.T) {
	c := listCache{kv: cache.NewMemoryCache(), ttl: time.Minute}
	started, finish, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.list(context.Background(), "a", ListOptions{}, func() (Page, error) { close(started); <-finish; return Page{Skills: []Metadata{{Name: "old"}}}, nil })
	}()
	<-started
	require.NoError(t, c.invalidate(context.Background(), "a"))
	close(finish)
	<-done
	page, err := c.list(context.Background(), "a", ListOptions{}, func() (Page, error) { return Page{Skills: []Metadata{{Name: "new"}}}, nil })
	require.NoError(t, err)
	require.Equal(t, "new", page.Skills[0].Name)
}

func TestListCacheErrorsAndDisable(t *testing.T) {
	c := listCache{kv: cache.NewMemoryCache(), ttl: time.Minute}
	failure := errors.New("backend down")
	_, err := c.list(context.Background(), "a", ListOptions{}, func() (Page, error) { return Page{}, failure })
	require.ErrorIs(t, err, failure)
	calls := 0
	load := func() (Page, error) { calls++; return Page{Skills: []Metadata{}}, nil }
	_, err = c.list(context.Background(), "a", ListOptions{}, load)
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	for _, disabled := range []listCache{{kv: cache.NewMemoryCache()}, {ttl: time.Minute}} {
		for i := 0; i < 2; i++ {
			_, err = disabled.list(context.Background(), "a", ListOptions{}, load)
			require.NoError(t, err)
		}
	}
	require.Equal(t, 5, calls)
}

type countingSkillS3 struct {
	*fakeS3
	lists atomic.Int32
	gets  atomic.Int32
}

func (s *countingSkillS3) ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	s.lists.Add(1)
	return s.fakeS3.ListObjectsV2(ctx, in, opts...)
}
func (s *countingSkillS3) GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	s.gets.Add(1)
	return s.fakeS3.GetObject(ctx, in, opts...)
}
func TestS3ListCacheAvoidsRemoteReads(t *testing.T) {
	client := &countingSkillS3{fakeS3: &fakeS3{objects: map[string][]byte{}}}
	store, err := NewS3Store(client, S3Config{Bucket: "test"})
	require.NoError(t, err)
	ctx := context.Background()
	_, err = store.Put(ctx, "a", bundle("review", "body"))
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		_, err = store.List(ctx, "a", ListOptions{})
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, client.lists.Load())
	require.EqualValues(t, 1, client.gets.Load())
	_, err = store.Get(ctx, "a", "review")
	require.NoError(t, err)
	require.EqualValues(t, 2, client.gets.Load()) // Contents are still read on demand.
}

func TestStoreListCacheMutationInvalidation(t *testing.T) {
	for kind, store := range stores(t) {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			page, err := store.List(ctx, "a", ListOptions{})
			require.NoError(t, err)
			require.Empty(t, page.Skills)
			for _, name := range []string{"one", "two", "three"} {
				_, err = store.Put(ctx, "a", bundle(name, "body"))
				require.NoError(t, err)
			}
			first, err := store.List(ctx, "a", ListOptions{Limit: 1})
			require.NoError(t, err)
			require.NotEmpty(t, first.NextCursor)
			next, err := store.List(ctx, "a", ListOptions{Limit: 1, Cursor: first.NextCursor})
			require.NoError(t, err)
			require.Len(t, next.Skills, 1)
			replacement := bundle(next.Skills[0].Name, "new body")
			replacement.Files["new.md"] = []byte("new resource")
			_, err = store.Put(ctx, "a", replacement)
			require.NoError(t, err)
			next, err = store.List(ctx, "a", ListOptions{Limit: 1, Cursor: first.NextCursor})
			require.NoError(t, err)
			require.Contains(t, next.Skills[0].Resources, "new.md")
			for _, name := range []string{"one", "two", "three"} {
				require.NoError(t, store.Delete(ctx, "a", name))
			}
			page, err = store.List(ctx, "a", ListOptions{})
			require.NoError(t, err)
			require.Empty(t, page.Skills)
		})
	}
}

type failingCache struct {
	*cache.MemoryCache
	fail bool
}

func (c *failingCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if c.fail {
		return errors.New("cache unavailable")
	}
	return c.MemoryCache.Set(ctx, key, value, ttl)
}
func TestStoreReportsInvalidationFailureAfterCommit(t *testing.T) {
	cache := &failingCache{MemoryCache: cache.NewMemoryCache()}
	store, err := NewFileStore(t.TempDir(), WithCache(cache))
	require.NoError(t, err)
	ctx := context.Background()
	_, err = store.List(ctx, "a", ListOptions{})
	require.NoError(t, err)
	cache.fail = true
	_, err = store.Put(ctx, "a", bundle("review", "body"))
	require.ErrorContains(t, err, "invalidation failed")
	// Persistence succeeded; callers must not assume a cache error rolled it back.
	_, err = store.Get(ctx, "a", "review")
	require.NoError(t, err)
}
func TestSharedCacheIsolatesStorageLocations(t *testing.T) {
	cache := cache.NewMemoryCache()
	a, err := NewFileStore(t.TempDir(), WithCache(cache))
	require.NoError(t, err)
	b, err := NewFileStore(t.TempDir(), WithCache(cache))
	require.NoError(t, err)
	ctx := context.Background()
	_, err = a.Put(ctx, "same", bundle("review", "body"))
	require.NoError(t, err)
	_, err = a.List(ctx, "same", ListOptions{})
	require.NoError(t, err)
	page, err := b.List(ctx, "same", ListOptions{})
	require.NoError(t, err)
	require.Empty(t, page.Skills)
}
func TestInvalidationSurvivesRequestCancellation(t *testing.T) {
	cache := cache.NewMemoryCache()
	c := listCache{kv: cache, ttl: time.Minute}
	ctx := context.Background()
	require.NoError(t, c.invalidate(ctx, "scope"))
	before, _, err := cache.Get(ctx, versionKey("scope"))
	require.NoError(t, err)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	require.NoError(t, invalidateList(canceled, c, "scope", nil))
	after, _, err := cache.Get(ctx, versionKey("scope"))
	require.NoError(t, err)
	require.NotEqual(t, string(before), string(after))
}
