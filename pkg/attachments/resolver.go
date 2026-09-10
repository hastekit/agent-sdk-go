// Package attachments resolves application-owned immutable files without putting
// their contents in conversation or workflow state.
package attachments

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

var (
	ErrInvalid    = errors.New("invalid attachment")
	ErrTooLarge   = errors.New("attachment exceeds size limit")
	ErrNotFound   = errors.New("attachment not found")
	ErrDenied     = errors.New("attachment access denied")
	ErrUnresolved = errors.New("attachment resolver is not configured")
)

// Ref identifies immutable content. Version may be empty if ID is immutable.
// It deliberately contains neither a URL nor storage credentials.
type Ref struct {
	ID      string `json:"id"`
	Version string `json:"version,omitempty"`
}

// Descriptor is authoritative metadata returned after authorization. Namespace
// MUST partition storage and tenants. Key and Version MUST identify immutable
// bytes; neither may be taken unchecked from client input. SHA256 is optional.
type Descriptor struct {
	Namespace string
	Key       string
	Version   string
	MediaType string
	Size      int64
	Filename  string
	SHA256    string
}

// Store is supplied by the host application and partitioned by namespace: the
// agent namespace, the same one AgentInput.Namespace names. Lookup must
// authorize the namespace on EVERY call, including cache hits. Open must return
// exactly the immutable object described by Lookup and honor context
// cancellation. Credentials belong in trusted server context, never in Ref.
type Store interface {
	Lookup(ctx context.Context, namespace string, ref Ref) (Descriptor, error)
	Open(context.Context, Descriptor) (io.ReadCloser, error)
}

type Config struct {
	CacheBytes         int64         // zero: 64 MiB; negative: no retained cache
	MaxFileBytes       int64         // zero: 20 MiB
	MaxConcurrentLoads int           // zero: 4
	LoadTimeout        time.Duration // zero: 30 seconds
}

type cacheKey struct {
	Namespace, Key, Version, MediaType, Hash string
	Size                                     int64
}
type entry struct {
	key  cacheKey
	data []byte
}

// Resolver is concurrency safe and should be shared across models and turns.
// Eviction/restarts can cause another storage read; the cache is not durable.
type Resolver struct {
	store  Store
	cfg    Config
	mu     sync.Mutex
	lru    *list.List
	cache  map[cacheKey]*list.Element
	used   int64
	loads  chan struct{}
	flight singleflight.Group
}

func NewResolver(store Store, cfg Config) *Resolver {
	if cfg.CacheBytes == 0 {
		cfg.CacheBytes = 64 << 20
	}
	if cfg.MaxFileBytes <= 0 {
		cfg.MaxFileBytes = 20 << 20
	}
	if cfg.MaxConcurrentLoads <= 0 {
		cfg.MaxConcurrentLoads = 4
	}
	if cfg.LoadTimeout <= 0 {
		cfg.LoadTimeout = 30 * time.Second
	}
	return &Resolver{store: store, cfg: cfg, lru: list.New(), cache: make(map[cacheKey]*list.Element), loads: make(chan struct{}, cfg.MaxConcurrentLoads)}
}

// Blob offers read-only access to cached bytes. Bytes never become a field of
// the serializable Ref or Descriptor.
type Blob struct {
	descriptor Descriptor
	data       []byte
}

func (b *Blob) Descriptor() Descriptor { return b.descriptor }
func (b *Blob) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(b.data)
	if err == nil && n != len(b.data) {
		err = io.ErrShortWrite
	}
	return int64(n), err
}

// Resolve returns the bytes behind ref as namespace may read them.
func (r *Resolver) Resolve(ctx context.Context, namespace string, ref Ref) (*Blob, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil || r.store == nil {
		return nil, ErrUnresolved
	}
	if ref.ID == "" {
		return nil, fmt.Errorf("%w: empty reference", ErrInvalid)
	}
	d, err := r.store.Lookup(ctx, namespace, ref)
	if err != nil {
		return nil, err
	}
	media, _, err := mime.ParseMediaType(d.MediaType)
	if err != nil || d.Namespace == "" || d.Key == "" || d.Size <= 0 {
		return nil, fmt.Errorf("%w: incomplete metadata", ErrInvalid)
	}
	d.MediaType = media
	if d.Size > r.cfg.MaxFileBytes {
		return nil, ErrTooLarge
	}
	key := cacheKey{d.Namespace, d.Key, d.Version, d.MediaType, d.SHA256, d.Size}
	if data := r.get(key); data != nil {
		return &Blob{d, data}, nil
	}
	// The shared load has its own deadline: cancelling one waiter must not
	// cancel another caller's read. Store context values remain available.
	ch := r.flight.DoChan(fmt.Sprintf("%#v", key), func() (any, error) {
		if data := r.get(key); data != nil {
			return data, nil
		}
		loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.cfg.LoadTimeout)
		defer cancel()
		select {
		case r.loads <- struct{}{}:
		case <-loadCtx.Done():
			return nil, loadCtx.Err()
		}
		defer func() { <-r.loads }()
		rd, err := r.store.Open(loadCtx, d)
		if err != nil {
			return nil, err
		}
		data, readErr := io.ReadAll(io.LimitReader(rd, d.Size+1))
		closeErr := rd.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if err := loadCtx.Err(); err != nil {
			return nil, err
		}
		if int64(len(data)) != d.Size {
			return nil, fmt.Errorf("%w: size does not match metadata", ErrInvalid)
		}
		if d.SHA256 != "" {
			sum := sha256.Sum256(data)
			if hex.EncodeToString(sum[:]) != d.SHA256 {
				return nil, fmt.Errorf("%w: checksum mismatch", ErrInvalid)
			}
		}
		if strings.HasPrefix(d.MediaType, "image/") && http.DetectContentType(data) != d.MediaType {
			return nil, fmt.Errorf("%w: image media type does not match content", ErrInvalid)
		}
		r.put(key, data)
		return data, nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-ch:
		if result.Err != nil {
			return nil, result.Err
		}
		return &Blob{d, result.Val.([]byte)}, nil
	}
}

func (r *Resolver) get(key cacheKey) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.cache[key]; e != nil {
		r.lru.MoveToFront(e)
		return e.Value.(entry).data
	}
	return nil
}
func (r *Resolver) put(key cacheKey, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := int64(len(data))
	if n > r.cfg.CacheBytes || r.cfg.CacheBytes < 0 {
		return
	}
	if e := r.cache[key]; e != nil {
		r.lru.MoveToFront(e)
		return
	}
	for r.used+n > r.cfg.CacheBytes {
		e := r.lru.Back()
		v := e.Value.(entry)
		delete(r.cache, v.key)
		r.used -= int64(len(v.data))
		r.lru.Remove(e)
	}
	r.cache[key] = r.lru.PushFront(entry{key, data})
	r.used += n
}
