package skills

import (
	"context"
	"time"
)

// KeyValueStore is the cache contract accepted by the built-in skill stores.
// Get returns found=false for a missing or expired key. Set replaces one key
// atomically and retains it for at most ttl; nonpositive ttl removes the key.
// Implementations must be safe for
// concurrent use. Cache values are opaque bytes, not skill policies.
type KeyValueStore interface {
	Get(context.Context, string) ([]byte, bool, error)
	Set(context.Context, string, []byte, time.Duration) error
	Delete(context.Context, string) error
}
