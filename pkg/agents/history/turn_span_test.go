package history

import (
	"context"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedClock is a persistence adapter whose clock returns a fixed sequence,
// standing in for a durable runtime's replayed reading.
type scriptedClock struct {
	*InMemoryConversationPersistence

	mu    sync.Mutex
	times []time.Time
	n     int
}

func newScriptedClock(times ...time.Time) *scriptedClock {
	return &scriptedClock{InMemoryConversationPersistence: NewInMemoryConversationPersistence(), times: times}
}

func (c *scriptedClock) Now(context.Context) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n >= len(c.times) {
		return c.times[len(c.times)-1]
	}
	at := c.times[c.n]
	c.n++
	return at
}

func mustParse(t *testing.T, v any) time.Time {
	t.Helper()
	s, ok := v.(string)
	require.True(t, ok, "meta value %v is not a string", v)
	parsed, err := time.Parse(time.RFC3339Nano, s)
	require.NoError(t, err)
	return parsed
}

// A turn is timed as a whole: when the message that opened it was sent, and
// when the agent answered. Both live on the run's meta, and both readings come
// from the adapter — which under a durable runtime is what makes them survive
// a replay.
func TestSaveRecordsTheTurnsSpan(t *testing.T) {
	ctx := context.Background()
	started := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	completed := started.Add(12 * time.Second)

	p := newScriptedClock(started, completed)
	cm := NewConversationManager(p)

	run, err := NewRun(ctx, cm, "ns", "thread-1", "")
	require.NoError(t, err)
	run.AddMessages(ctx, userBundle("ada", "hello"))
	require.NoError(t, run.SaveMessages(ctx))

	loaded, err := p.LoadMessages(ctx, "ns", "thread-1", run.GetRunID())
	require.NoError(t, err)
	require.Len(t, loaded, 1)

	assert.Equal(t, started, mustParse(t, loaded[0].Meta[agentstate.StartedAtMetaKey]),
		"the run is opened when the turn arrives")
	assert.Equal(t, completed, mustParse(t, loaded[0].Meta[agentstate.CompletedAtMetaKey]))
}

// A run that paused for an approval and continued keeps the time it was
// opened with: the turn began when the user asked, not when the answer they
// were waiting on came back. Only the completion moves.
func TestAResumedTurnKeepsItsOriginalStart(t *testing.T) {
	ctx := context.Background()
	opened := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	paused := opened.Add(3 * time.Second)
	answered := opened.Add(2 * time.Hour)

	p := newScriptedClock(opened, paused, answered)
	cm := NewConversationManager(p)

	run, err := NewRun(ctx, cm, "ns", "thread-1", "")
	require.NoError(t, err)
	run.AddMessages(ctx, userBundle("ada", "book me a flight"))
	require.NoError(t, run.SaveMessages(ctx))

	// A fresh manager for the resuming request, as a second call would build.
	resumed, err := NewRun(ctx, cm, "ns", "thread-1", "")
	require.NoError(t, err)
	resumed.AddMessages(ctx, userBundle("ada", "yes, go ahead"))
	require.NoError(t, resumed.SaveMessages(ctx))

	loaded, err := p.LoadMessages(ctx, "ns", "thread-1", resumed.GetRunID())
	require.NoError(t, err)
	require.NotEmpty(t, loaded)

	last := loaded[len(loaded)-1]
	assert.Equal(t, opened, mustParse(t, last.Meta[agentstate.StartedAtMetaKey]),
		"the pause is inside the span, not a gap between two of them")
	assert.Equal(t, answered, mustParse(t, last.Meta[agentstate.CompletedAtMetaKey]))
}

// A turn that opens after the previous one finished is its own span.
func TestANewTurnStartsItsOwnSpan(t *testing.T) {
	ctx := context.Background()
	firstOpened := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	firstDone := firstOpened.Add(4 * time.Second)
	secondOpened := firstOpened.Add(time.Hour)
	secondDone := secondOpened.Add(2 * time.Second)

	p := newScriptedClock(firstOpened, firstDone, secondOpened, secondDone)
	cm := NewConversationManager(p)

	run, err := NewRun(ctx, cm, "ns", "thread-1", "")
	require.NoError(t, err)
	run.AddMessages(ctx, userBundle("ada", "hello"))
	run.RunState.TransitionToComplete()
	require.NoError(t, run.SaveMessages(ctx))

	next, err := NewRun(ctx, cm, "ns", "thread-1", "")
	require.NoError(t, err)
	next.AddMessages(ctx, userBundle("ada", "another question"))
	require.NoError(t, next.SaveMessages(ctx))

	loaded, err := p.LoadMessages(ctx, "ns", "thread-1", next.GetRunID())
	require.NoError(t, err)
	require.NotEmpty(t, loaded)

	last := loaded[len(loaded)-1]
	assert.Equal(t, secondOpened, mustParse(t, last.Meta[agentstate.StartedAtMetaKey]))
	assert.Equal(t, secondDone, mustParse(t, last.Meta[agentstate.CompletedAtMetaKey]))
}

// A run saved before the span was recorded has no start to carry forward, so
// the resuming request opens one.
func TestAResumeWithNoRecordedStartOpensOne(t *testing.T) {
	ctx := context.Background()
	opened := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

	p := newScriptedClock(opened)
	cm := NewConversationManager(p)

	// A row written by an older version: messages and state, no span.
	require.NoError(t, p.SaveMessages(ctx, "ns", "run-old", "", "thread-1", "conv-1",
		[]Message{userBundle("ada", "asked before this existed")}, map[string]any{}))

	resumed, err := NewRun(ctx, cm, "ns", "thread-1", "")
	require.NoError(t, err)
	resumed.AddMessages(ctx, userBundle("ada", "and again"))
	require.NoError(t, resumed.SaveMessages(ctx))

	loaded, err := p.LoadMessages(ctx, "ns", "thread-1", resumed.GetRunID())
	require.NoError(t, err)
	require.NotEmpty(t, loaded)

	last := loaded[len(loaded)-1]
	assert.Equal(t, opened, mustParse(t, last.Meta[agentstate.StartedAtMetaKey]))
}

// The span survives a restart, like the rest of a run's meta.
func TestTheSpanRoundTripsThroughTheFileAdapter(t *testing.T) {
	dir, err := os.MkdirTemp("", "turn-span-*")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })

	ctx := context.Background()
	p, err := NewFileConversationPersistence(dir)
	require.NoError(t, err)
	cm := NewConversationManager(p)

	before := time.Now().Add(-time.Second)

	run, err := NewRun(ctx, cm, "ns", "thread-1", "")
	require.NoError(t, err)
	run.AddMessages(ctx, userBundle("ada", "hello"))
	require.NoError(t, run.SaveMessages(ctx))

	reopened, err := NewFileConversationPersistence(dir)
	require.NoError(t, err)

	loaded, err := reopened.LoadMessages(ctx, "ns", "thread-1", run.GetRunID())
	require.NoError(t, err)
	require.NotEmpty(t, loaded)

	started := mustParse(t, loaded[0].Meta[agentstate.StartedAtMetaKey])
	completed := mustParse(t, loaded[0].Meta[agentstate.CompletedAtMetaKey])
	assert.True(t, started.After(before))
	assert.False(t, completed.Before(started), "the agent cannot answer before it was asked")
}
