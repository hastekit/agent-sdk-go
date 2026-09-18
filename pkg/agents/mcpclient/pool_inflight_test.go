package mcpclient

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A connection with a call on it is not idle, however long ago it was handed
// out.
//
// This is the bug the counting exists for: lastUsed was written when a call
// started and never again, so a tool that ran past the idle timeout looked
// abandoned and the sweep closed the connection under it.
func TestPoolEntry_WithWorkOnItIsNeverIdle(t *testing.T) {
	entry := &poolEntry{}

	started := time.Now()
	release := entry.acquire()

	// The sweep comes round an hour later and the call is still going. Time
	// alone says abandoned; only the count says otherwise, which is the whole
	// point — lastUsed was written when the call began and not since.
	assert.False(t, entry.idle(started.Add(time.Hour), time.Minute),
		"a long tool call is work in progress, not an abandoned connection")

	release()
	assert.True(t, entry.idle(time.Now().Add(2*time.Minute), time.Minute),
		"and once the call is done it ages out like anything else")
}

// Idleness is measured from the end of the work. A long call that came back
// already stale would be closed by the next sweep, having just proved the
// connection is worth keeping.
func TestPoolEntry_AgesFromTheEndOfTheWork(t *testing.T) {
	entry := &poolEntry{lastUsed: time.Now().Add(-time.Hour)}

	entry.acquire()()

	assert.False(t, entry.idle(time.Now(), time.Minute))
}

func TestPoolEntry_ReleaseIsIdempotent(t *testing.T) {
	entry := &poolEntry{lastUsed: time.Now()}

	release := entry.acquire()
	release()
	release()

	entry.mu.Lock()
	defer entry.mu.Unlock()
	assert.Zero(t, entry.inFlight, "a release called twice must not lend the connection out in the negative")
}

func TestPoolEntry_CountsConcurrentCallers(t *testing.T) {
	entry := &poolEntry{}

	started := time.Now()
	var releases []func()
	for range 5 {
		releases = append(releases, entry.acquire())
	}

	// Long after any of them started, and one at a time: the connection stays
	// in use until the last caller is done with it.
	for i, release := range releases {
		assert.False(t, entry.idle(started.Add(time.Hour), time.Minute),
			"still %d callers on it", len(releases)-i)
		release()
	}

	assert.True(t, entry.idle(time.Now().Add(2*time.Minute), time.Minute))
}

// The other half of the bug: the sweep closed connections while holding the
// pool's lock, and a session's Close waits for its in-flight requests — so one
// slow server stalled every conversation in the process, on servers none of
// them were talking to.
//
// The fix is that detaching and closing are separate steps: takeIdle does the
// first under the lock and hands the entries back, and only the caller closes.
// That is what this pins — a takeIdle that closed anything itself would be
// holding the pool while it did.
func TestPool_TakeIdleDetachesWithoutClosing(t *testing.T) {
	pool := &connectionPool{
		connections: map[string]*poolEntry{},
		idleTimeout: time.Minute,
		stopCleanup: make(chan struct{}),
	}

	stale := &poolEntry{lastUsed: time.Now().Add(-time.Hour)}
	fresh := &poolEntry{lastUsed: time.Now()}
	pool.connections["stale"] = stale
	pool.connections["fresh"] = fresh

	taken := pool.takeIdle(time.Now())

	require.Len(t, taken, 1, "closing is the caller's to do, outside the lock")
	assert.Same(t, stale, taken[0])

	pool.mu.RLock()
	defer pool.mu.RUnlock()
	assert.NotContains(t, pool.connections, "stale", "detached, so nobody is handed it again")
	assert.Contains(t, pool.connections, "fresh")
}

// And the lock is free the moment it returns, whatever the caller then does
// with the entries.
func TestPool_TakeIdleLeavesTheLockFree(t *testing.T) {
	pool := &connectionPool{
		connections: map[string]*poolEntry{"stale": {lastUsed: time.Now().Add(-time.Hour)}},
		idleTimeout: time.Millisecond,
		stopCleanup: make(chan struct{}),
	}

	taken := pool.takeIdle(time.Now())
	require.Len(t, taken, 1)

	acquired := make(chan struct{})
	go func() {
		defer close(acquired)
		pool.mu.Lock()
		pool.mu.Unlock()
	}()

	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("the pool is still held; every other conversation would be stuck here")
	}
}

// A connection with work on it survives the sweep entirely.
func TestPool_SweepSkipsConnectionsInUse(t *testing.T) {
	pool := &connectionPool{
		connections: map[string]*poolEntry{},
		idleTimeout: time.Minute,
		stopCleanup: make(chan struct{}),
	}

	busy := &poolEntry{}
	release := busy.acquire()
	pool.connections["busy"] = busy

	idle := &poolEntry{lastUsed: time.Now()}
	pool.connections["idle"] = idle

	// An hour on. Both look abandoned by the clock; one has a call on it.
	for _, entry := range pool.takeIdle(time.Now().Add(time.Hour)) {
		entry.close()
	}

	pool.mu.RLock()
	_, keptBusy := pool.connections["busy"]
	_, keptIdle := pool.connections["idle"]
	pool.mu.RUnlock()

	assert.True(t, keptBusy, "a connection carrying a request is not swept")
	assert.False(t, keptIdle, "one carrying nothing still is")

	release()
	require.True(t, busy.idle(time.Now().Add(2*time.Minute), time.Minute))
}
