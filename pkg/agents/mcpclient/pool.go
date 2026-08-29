package mcpclient

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var (
	// globalPool is the package-level connection pool shared across all MCPClient instances.
	// Since temporal/restate workers are long-lived processes, this pool is shared across
	// all activity/handler executions on the same worker.
	globalPool = newConnectionPool(5 * time.Minute)
)

// SchemaCache defines the interface for caching MCP tool schemas.
// Implementations can use Redis, in-memory stores, or any other backing store.
type SchemaCache interface {
	// Get retrieves cached tool schemas by key. Returns nil, false on cache miss.
	Get(ctx context.Context, key string) (*CachedToolEntry, bool)
	// Set stores tool schemas with the given key, for at most ttl.
	//
	// A store that can expire keys itself should use ttl; one that cannot may
	// ignore it, since the entry carries its own ExpiresAt and is checked on
	// the way out. A ttl of zero means no expiry — nobody, server or caller,
	// put a life on this entry.
	Set(ctx context.Context, key string, entry *CachedToolEntry, ttl time.Duration)
	// Delete removes a cached entry by key.
	Delete(ctx context.Context, key string)
	// Clear removes all cached entries.
	Clear(ctx context.Context)
}

// CachedToolEntry stores cached MCP tool schemas.
type CachedToolEntry struct {
	Tools []*mcp.Tool `json:"tools"`
	Meta  mcp.Meta    `json:"meta,omitempty"`

	// CacheScope is what the server said about sharing this listing —
	// "public", "private", or empty from a server too old to have been asked.
	// Recorded so a stored entry can be read back and understood on its own.
	CacheScope string `json:"cache_scope,omitempty"`

	// ExpiresAt is when this entry stops being usable. Checked on read as well
	// as handed to Set, because a SchemaCache may be a store with no expiry of
	// its own, or one shared with a writer that gave the key a longer life.
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// expired reports whether this entry may no longer be served. An entry with no
// ExpiresAt was written before the field existed and is taken at face value.
func (e *CachedToolEntry) expired() bool {
	if e == nil {
		return true
	}
	return !e.ExpiresAt.IsZero() && time.Now().After(e.ExpiresAt)
}

// poolEntry holds a live MCP connection.
type poolEntry struct {
	client   *mcp.ClientSession
	lastUsed time.Time
	mu       sync.Mutex
}

// connectionPool manages reusable MCP connections keyed by endpoint+transport+headers.
type connectionPool struct {
	mu          sync.RWMutex
	connections map[string]*poolEntry
	idleTimeout time.Duration
	stopCleanup chan struct{}
	stopOnce    sync.Once
}

func newConnectionPool(idleTimeout time.Duration) *connectionPool {
	p := &connectionPool{
		connections: make(map[string]*poolEntry),
		idleTimeout: idleTimeout,
		stopCleanup: make(chan struct{}),
	}
	go p.cleanupLoop()
	return p
}

// Checkout returns an existing healthy connection or creates a new one. For a
// stdio server that connection is a running child process, so pooling is what
// keeps a tool call from paying a process launch every time.
// The mcp-go SSE client supports concurrent CallTool calls via JSON-RPC request IDs,
// so a single connection per server is sufficient.
//
// IMPORTANT: Pool-managed connections use context.Background() for their SSE stream
// lifecycle, NOT the caller's context. This is critical because in Temporal/Restate,
// the activity/handler context is cancelled after the function returns. If the SSE
// reader goroutine were tied to that context, it would die after the first tool call,
// making the pooled connection unusable for subsequent calls.
func (p *connectionPool) Checkout(ctx context.Context, conn serverConn) (*mcp.ClientSession, error) {
	key := conn.key()

	p.mu.RLock()
	entry, exists := p.connections[key]
	p.mu.RUnlock()

	if exists {
		entry.mu.Lock()
		entry.lastUsed = time.Now()
		cli := entry.client
		entry.mu.Unlock()

		if cli != nil {
			return cli, nil
		}
	}

	// Create new connection using context.Background() so the SSE stream — or,
	// for stdio, the server process itself — survives beyond the caller's
	// context (e.g. a Temporal activity context).
	cli, err := createConnection(context.Background(), conn)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	p.connections[key] = &poolEntry{
		client:   cli,
		lastUsed: time.Now(),
	}
	p.mu.Unlock()

	return cli, nil
}

// Remove removes a connection from the pool (e.g., when it's known to be dead).
func (p *connectionPool) Remove(conn serverConn) {
	key := conn.key()
	p.mu.Lock()
	if entry, ok := p.connections[key]; ok {
		entry.mu.Lock()
		if entry.client != nil {
			entry.client.Close()
		}
		entry.mu.Unlock()
		delete(p.connections, key)
	}
	p.mu.Unlock()
}

func (p *connectionPool) cleanupLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			p.cleanupIdle()
		case <-p.stopCleanup:
			return
		}
	}
}

func (p *connectionPool) cleanupIdle() {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()

	for key, entry := range p.connections {
		entry.mu.Lock()
		if now.Sub(entry.lastUsed) > p.idleTimeout {
			if entry.client != nil {
				entry.client.Close()
			}
			delete(p.connections, key)
			slog.Debug("MCP connection pool: closed idle connection", slog.String("key", key))
		}
		entry.mu.Unlock()
	}
}

// Close closes all connections and stops the cleanup goroutine.
func (p *connectionPool) Close() {
	p.stopOnce.Do(func() {
		close(p.stopCleanup)
		p.mu.Lock()
		defer p.mu.Unlock()
		for key, entry := range p.connections {
			entry.mu.Lock()
			if entry.client != nil {
				entry.client.Close()
			}
			entry.mu.Unlock()
			delete(p.connections, key)
		}
	})
}

// createConnection establishes a new MCP connection (Connect performs
// the initialize handshake; no separate ListTools).
func createConnection(ctx context.Context, conn serverConn) (*mcp.ClientSession, error) {
	session, err := connect(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("failed to connect MCP client: %w", err)
	}
	return session, nil
}
