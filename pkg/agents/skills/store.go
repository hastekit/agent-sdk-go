// Package skills persists complete skill bundles independently of agent configuration.
package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hastekit/agent-sdk-go/pkg/cache"
)

var (
	ErrNotFound = errors.New("skill not found")
	ErrInvalid  = errors.New("invalid skill")
	ErrTooLarge = errors.New("skill exceeds size limit")
)

const MaxBundleBytes = 10 << 20
const MaxFiles = 100

// Bundle contains SKILL.md and paths relative to the skill folder. JSON encodes
// file bytes as base64. Put atomically replaces the complete named bundle.
type Bundle struct {
	Files map[string][]byte `json:"files"`
}
type Metadata struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Resources   []string `json:"resources"`
	// FileLocation is where SKILL.md lives, for stores read from a filesystem.
	// Stores leave it empty; the client copies it to agents.Skill.FileLocation.
	FileLocation string `json:"fileLocation,omitempty"`
}
type ListOptions struct {
	Limit  int
	Cursor string
}
type Page struct {
	Skills     []Metadata `json:"skills"`
	NextCursor string     `json:"nextCursor,omitempty"`
}

// Store lists and reads skill bundles by namespace. It is everything a Client
// needs, so built-in skills, filters and custom backends implement only this.
// It mirrors mcpclient.MCPServerConfigStore.
//
// The empty namespace holds global skills: developer-owned, listed for every
// namespace, and winning over a namespace's own skill with the same name. Any
// other namespace holds that namespace's own skills. List pages through one
// namespace only; Get returns ErrNotFound for a name the namespace lacks.
// Implementations must be safe for concurrent callers and honor cancellation.
type Store interface {
	Get(ctx context.Context, namespace, name string) (Bundle, error)
	List(ctx context.Context, namespace string, opts ListOptions) (Page, error)
}

// MutableStore is a Store that also saves skills, as
// mcpclient.MutableMCPServerConfigStore is for MCP servers. Implementations must isolate
// namespaces, atomically replace bundles, return ErrNotFound for absent Get
// calls, make Delete idempotent, and be safe for concurrent callers.
//
// Writing the empty namespace changes every agent's global skills, so only
// trusted server code may do it. NewHandler refuses it, whatever namespace the
// request resolves to. Policies and authorization belong to the host, never
// uploaded skill content.
type MutableStore interface {
	Store
	Put(ctx context.Context, namespace string, bundle Bundle) (Metadata, error)
	Delete(ctx context.Context, namespace, name string) error
}

// StoreOption configures the built-in stores.
type StoreOption func(*storeOptions)
type storeOptions struct {
	cache KeyValueStore
	ttl   time.Duration
}

// WithCache injects a key-value store for raw skill listing pages. The default
// is a private in-memory cache. Pass nil to disable listing caching.
func WithCache(cache KeyValueStore) StoreOption {
	return func(o *storeOptions) { o.cache = cache }
}

// WithCacheTTL sets the maximum lifetime of cached listing pages. The default
// is one minute. A nonpositive TTL disables caching.
func WithCacheTTL(ttl time.Duration) StoreOption {
	return func(o *storeOptions) { o.ttl = ttl }
}
func configureStore(opts []StoreOption) listCache {
	o := storeOptions{cache: cache.NewMemoryCache(), ttl: listCacheTTL}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return listCache{kv: o.cache, ttl: o.ttl}
}

// Invalidation must still run if the request was canceled after committing a
// write. Report failures: the backing write may have succeeded while other
// replicas still hold cached listings until their TTL expires.
func invalidateList(ctx context.Context, cache listCache, scope string, writeErr error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := cache.invalidate(ctx, scope); err != nil {
		return errors.Join(writeErr, fmt.Errorf("skill write may have been applied; listing cache invalidation failed: %w", err))
	}
	return writeErr
}

func validName(s string) bool {
	return s != "" && len(s) <= 128 && s == strings.TrimSpace(s) && fs.ValidPath(s) && !strings.ContainsAny(s, "/\\\x00") && s != "."
}

// namespaceKey names a namespace's storage. The empty namespace is the global
// one; a blank but nonempty namespace is rejected so it cannot pass for it.
func namespaceKey(ctx context.Context, ns string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if ns != "" && strings.TrimSpace(ns) == "" {
		return "", fmt.Errorf("%w: blank namespace", ErrInvalid)
	}
	sum := sha256.Sum256([]byte(ns))
	return hex.EncodeToString(sum[:]), nil
}
func skillKey(name string) (string, error) {
	if !validName(name) {
		return "", fmt.Errorf("%w: name", ErrInvalid)
	}
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:]) + ".json", nil
}
func limit(n int) int {
	if n <= 0 {
		return 50
	}
	if n > 200 {
		return 200
	}
	return n
}

// Validate requires explicit name and description in SKILL.md frontmatter.
// It rejects unsafe resource paths and bounds total decoded content.
func Validate(b Bundle) (Metadata, error) {
	var m Metadata
	if len(b.Files) == 0 || len(b.Files) > MaxFiles {
		return m, fmt.Errorf("%w: expected 1–100 files", ErrInvalid)
	}
	total := 0
	for path, data := range b.Files {
		if len(path) > 1024 || !fs.ValidPath(path) || path == "." || strings.ContainsAny(path, "\\\x00") {
			return m, fmt.Errorf("%w: file path %q", ErrInvalid, path)
		}
		total += len(data)
		if total > MaxBundleBytes {
			return m, ErrTooLarge
		}
		if path != "SKILL.md" {
			m.Resources = append(m.Resources, path)
		}
	}
	data, ok := b.Files["SKILL.md"]
	if !ok || !utf8.Valid(data) {
		return m, fmt.Errorf("%w: UTF-8 SKILL.md required", ErrInvalid)
	}
	fm, _, err := parseSkillFrontmatter(data)
	if err != nil {
		return m, fmt.Errorf("%w: frontmatter: %v", ErrInvalid, err)
	}
	if !validName(fm.Name) || strings.TrimSpace(fm.Description) == "" || len(fm.Description) > 8192 {
		return m, fmt.Errorf("%w: name and description required", ErrInvalid)
	}
	m.Name = fm.Name
	m.Description = strings.TrimSpace(fm.Description)
	sort.Strings(m.Resources)
	return m, nil
}
