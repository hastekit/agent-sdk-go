package agents

import (
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func take(g *messageIDs, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = g.Next()
	}
	return out
}

// The point of deriving rather than drawing: a replay re-runs the loop in the
// same order and gets the same ids back, without anything having been recorded.
func TestTheSameTurnMintsTheSameIDs(t *testing.T) {
	first := take(newMessageIDs("run-1", "turn-1"), 5)
	replay := take(newMessageIDs("run-1", "turn-1"), 5)

	assert.Equal(t, first, replay)
}

// They are real uuids, in canonical form.
func TestDerivedIDsAreUUIDs(t *testing.T) {
	for _, id := range take(newMessageIDs("run-1", "turn-1"), 3) {
		parsed, err := uuid.Parse(id)
		require.NoError(t, err, "%q is not a uuid", id)
		assert.Equal(t, id, parsed.String())
		assert.Equal(t, uuid.Version(5), parsed.Version(), "derived from the seed, not drawn")
	}
}

func TestEachIDInATurnIsDistinct(t *testing.T) {
	ids := take(newMessageIDs("run-1", "turn-1"), 100)

	seen := map[string]bool{}
	for i, id := range ids {
		assert.False(t, seen[id], "id %d repeats: %q", i, id)
		seen[id] = true
	}
}

func TestDifferentRunsMintDifferentIDs(t *testing.T) {
	assert.NotEqual(t,
		take(newMessageIDs("run-1", "turn-1"), 3),
		take(newMessageIDs("run-2", "turn-1"), 3))
}

// A run that pauses for an approval keeps its id, and the resumed execution
// starts counting from zero again. The turn is what tells the two apart — with
// the run alone in the seed, the second segment would mint ids the first
// already used.
func TestAResumedTurnDoesNotRepeatTheFirstSegmentsIDs(t *testing.T) {
	opening := take(newMessageIDs("run-1", "turn-1"), 3)
	resumed := take(newMessageIDs("run-1", "turn-2"), 3)

	for _, id := range resumed {
		assert.NotContains(t, opening, id)
	}
}

// Nothing in the loop mints concurrently today, but the counter is shared
// state and a tool loop is the obvious place for that to change.
func TestTheCounterIsSafeUnderConcurrency(t *testing.T) {
	g := newMessageIDs("run-1", "turn-1")

	var mu sync.Mutex
	seen := map[string]bool{}

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := g.Next()
			mu.Lock()
			defer mu.Unlock()
			seen[id] = true
		}()
	}
	wg.Wait()

	assert.Len(t, seen, 50, "every draw is its own id")
}
