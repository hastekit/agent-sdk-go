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
	//
	// The timeout is how long a connection may sit doing nothing, not how long
	// it may live: a call in flight holds it open however long it takes. See
	// poolEntry.inFlight.
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

	// inFlight counts the callers currently using this session — a tool call
	// waiting on the server, and in time a background task's wait polling
	// tasks/get. A connection with work on it is not idle, however long ago it
	// was handed out: a tool that takes ten minutes updates lastUsed twice,
	// at each end, and nothing in between.
	//
	// Closing under such a call is what this exists to prevent. The session's
	// Close blocks until in-flight requests finish, so a pool that closed one
	// would then sit inside Close holding its own lock, and every other
	// conversation would block trying to check a connection out.
	inFlight int

	mu sync.Mutex
}

// idle reports whether nothing is using this connection and nothing has for
// longer than timeout.
func (e *poolEntry) idle(now time.Time, timeout time.Duration) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.inFlight == 0 && now.Sub(e.lastUsed) > timeout
}

// acquire marks one caller as using the connection, and returns the function
// that says they are done.
func (e *poolEntry) acquire() func() {
	e.mu.Lock()
	e.inFlight++
	e.lastUsed = time.Now()
	e.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			e.mu.Lock()
			e.inFlight--
			// Idleness is measured from the end of the work, not the start of
			// it — otherwise a long call comes back already stale and the next
			// sweep closes a connection that has just proved it is useful.
			e.lastUsed = time.Now()
			e.mu.Unlock()
		})
	}
}

func (e *poolEntry) close() {
	e.mu.Lock()
	client := e.client
	e.client = nil
	e.mu.Unlock()

	if client != nil {
		client.Close()
	}
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
// The returned release must be called when the caller is done with the
// session. Until it is, the connection counts as in use and is never swept as
// idle — which is what keeps a long tool call, or a background task's wait,
// from having the connection closed under it.
func (p *connectionPool) Checkout(ctx context.Context, conn serverConn) (*mcp.ClientSession, func(), error) {
	key := conn.key()

	p.mu.RLock()
	entry, exists := p.connections[key]
	p.mu.RUnlock()

	if exists {
		entry.mu.Lock()
		cli := entry.client
		entry.mu.Unlock()

		if cli != nil {
			return cli, entry.acquire(), nil
		}
	}

	// Create new connection using context.Background() so the SSE stream — or,
	// for stdio, the server process itself — survives beyond the caller's
	// context (e.g. a Temporal activity context).
	cli, err := createConnection(context.Background(), conn)
	if err != nil {
		return nil, nil, err
	}

	fresh := &poolEntry{client: cli, lastUsed: time.Now()}
	release := fresh.acquire()

	p.mu.Lock()
	// Another caller may have raced us to it. Theirs is already in the map and
	// may have work on it, so this one is the spare: hand back the one that is
	// in the pool and close ours rather than replacing a connection somebody
	// is using.
	if existing, ok := p.connections[key]; ok && existing.client != nil {
		p.mu.Unlock()
		release()
		go fresh.close()
		return existing.client, existing.acquire(), nil
	}
	p.connections[key] = fresh
	p.mu.Unlock()

	return cli, release, nil
}

// Remove takes a connection out of the pool, for one believed dead.
//
// Detaching is immediate, so nobody else is handed it again. Closing is not:
// the session's Close waits for whatever is still in flight on it, and the
// caller here is a tool call wanting to retry — it should not wait on somebody
// else's request to a server that has probably stopped answering.
func (p *connectionPool) Remove(conn serverConn) {
	key := conn.key()

	p.mu.Lock()
	entry, ok := p.connections[key]
	delete(p.connections, key)
	p.mu.Unlock()

	if ok {
		go entry.close()
	}
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

// cleanupIdle closes connections nothing has used for a while.
//
// It closes outside the pool's lock, always. A session's Close waits for its
// in-flight requests, so closing under the lock would park the sweep inside
// Close with the pool held — and every conversation in the process would block
// checking a connection out, on a server none of them were talking to.
// Entries with work on them are not swept at all; this is the second line.
func (p *connectionPool) cleanupIdle() {
	for _, entry := range p.takeIdle(time.Now()) {
		entry.close()
	}
}

// takeIdle detaches the connections nothing has used for a while and hands
// them back for the caller to close.
//
// Detaching and closing are separate on purpose, and this is where the split
// lives: a session's Close waits for its in-flight requests, so closing inside
// here would park the sweep with the pool held, and every conversation in the
// process would block checking a connection out — on a server none of them
// were talking to. Nothing that takes the pool's lock may also close.
func (p *connectionPool) takeIdle(now time.Time) []*poolEntry {
	p.mu.Lock()
	defer p.mu.Unlock()

	var idle []*poolEntry
	for key, entry := range p.connections {
		if !entry.idle(now, p.idleTimeout) {
			continue
		}
		delete(p.connections, key)
		idle = append(idle, entry)
		slog.Debug("MCP connection pool: closing idle connection", slog.String("key", key))
	}
	return idle
}

// Close closes all connections and stops the cleanup goroutine.
func (p *connectionPool) Close() {
	p.stopOnce.Do(func() {
		close(p.stopCleanup)

		p.mu.Lock()
		closing := make([]*poolEntry, 0, len(p.connections))
		for key, entry := range p.connections {
			closing = append(closing, entry)
			delete(p.connections, key)
		}
		p.mu.Unlock()

		// Outside the lock, like the sweep — and waited for, unlike Remove:
		// this is shutdown, and a server with a request still in flight
		// deserves the chance to finish it.
		for _, entry := range closing {
			entry.close()
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
