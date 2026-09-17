// Package cache provides byte-oriented cache implementations. Consumers define
// their own interfaces; this package depends on neither MCP nor skill storage.
package cache

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

type memoryCacheEntry struct {
	value   []byte
	expires time.Time
}

// MemoryCache stores at most 256 keys, evicting the earliest-expiring entry.
// Use one shared instance to share invalidation between stores in one process.
type MemoryCache struct {
	mu      sync.Mutex
	entries map[string]memoryCacheEntry
}

func NewMemoryCache() *MemoryCache { return &MemoryCache{entries: make(map[string]memoryCacheEntry)} }
func (c *MemoryCache) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return nil, false, nil
	}
	if !time.Now().Before(entry.expires) {
		delete(c.entries, key)
		return nil, false, nil
	}
	return slices.Clone(entry.value), true, nil
}
func (c *MemoryCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if ttl <= 0 {
		delete(c.entries, key)
		return nil
	}
	if c.entries == nil {
		c.entries = make(map[string]memoryCacheEntry)
	}
	if _, ok := c.entries[key]; !ok && len(c.entries) >= 256 {
		var oldest string
		var expires time.Time
		for k, v := range c.entries {
			if expires.IsZero() || v.expires.Before(expires) {
				oldest, expires = k, v.expires
			}
		}
		delete(c.entries, oldest)
	}
	c.entries[key] = memoryCacheEntry{slices.Clone(value), time.Now().Add(ttl)}
	return nil
}
func (c *MemoryCache) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
	return nil
}

// RedisCache adapts an application-owned Redis client for consumer-owned cache interfaces. Use the
// same prefix on replicas sharing a backing store; use distinct prefixes
// for unrelated deployments. The caller closes the client.
type RedisCache struct {
	client redis.Cmdable
	prefix string
}

func NewRedisCache(client redis.Cmdable, prefix string) (*RedisCache, error) {
	if client == nil {
		return nil, fmt.Errorf("cache: Redis client required")
	}
	if prefix == "" {
		prefix = "hastekit:cache:"
	}
	return &RedisCache{client: client, prefix: prefix}, nil
}
func (c *RedisCache) Get(ctx context.Context, key string) ([]byte, bool, error) {
	data, err := c.client.Get(ctx, c.prefix+key).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	return data, err == nil, err
}
func (c *RedisCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if ttl <= 0 {
		return c.Delete(ctx, key)
	}
	return c.client.Set(ctx, c.prefix+key, value, ttl).Err()
}
func (c *RedisCache) Delete(ctx context.Context, key string) error {
	return c.client.Del(ctx, c.prefix+key).Err()
}
