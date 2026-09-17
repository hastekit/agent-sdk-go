package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

const listCacheTTL = time.Minute

type listCache struct {
	kv  KeyValueStore
	ttl time.Duration
}
type cachedListPage struct {
	Page    Page      `json:"page"`
	Expires time.Time `json:"expires"`
}

func cacheDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func versionKey(scope string) string { return "list:v1:" + cacheDigest(scope) + ":version" }

func (c listCache) list(ctx context.Context, scope string, opts ListOptions, load func() (Page, error)) (Page, error) {
	if err := ctx.Err(); err != nil {
		return Page{}, err
	}
	if c.kv == nil || c.ttl <= 0 {
		return load()
	}
	version, found, err := c.kv.Get(ctx, versionKey(scope))
	if err != nil {
		return Page{}, err
	}
	if !found {
		// Never reuse a version after expiry/eviction. Initialization precedes the
		// backing read, so competing initializers cannot publish stale data into
		// the new version even without compare-and-swap support in the KV store.
		version = []byte(uuid.NewString())
		if err = c.kv.Set(ctx, versionKey(scope), version, c.ttl); err != nil {
			return Page{}, err
		}
	}
	params, _ := json.Marshal(struct {
		Cursor string
		Limit  int
	}{opts.Cursor, limit(opts.Limit)})
	key := "list:v1:" + cacheDigest(scope) + ":" + string(version) + ":" + cacheDigest(string(params))
	data, found, err := c.kv.Get(ctx, key)
	if err != nil {
		return Page{}, err
	}
	if found {
		var cached cachedListPage
		if json.Unmarshal(data, &cached) == nil && time.Now().Before(cached.Expires) {
			return cached.Page, nil
		}
	}
	page, err := load()
	if err != nil {
		return page, err
	}
	if err = ctx.Err(); err != nil {
		return Page{}, err
	}
	data, err = json.Marshal(cachedListPage{page, time.Now().Add(c.ttl)})
	if err != nil {
		return Page{}, err
	}
	// Invalidations switch the version key. A racing load can only publish to
	// its old version, which subsequent readers cannot reach. Old pages expire.
	if err = c.kv.Set(ctx, key, data, c.ttl); err != nil {
		return Page{}, err
	}
	return page, nil
}
func (c listCache) invalidate(ctx context.Context, scope string) error {
	if c.kv == nil || c.ttl <= 0 {
		return nil
	}
	return c.kv.Set(ctx, versionKey(scope), []byte(uuid.NewString()), c.ttl)
}
