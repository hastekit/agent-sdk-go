package agents

import (
	"strconv"
	"sync"

	"github.com/google/uuid"
)

// messageIDNamespace scopes the derived ids to this SDK, so a seed that
// happens to collide with something else's produces different uuids.
var messageIDNamespace = uuid.MustParse("cfcb91e0-1bf7-491f-bb73-ca91e443aa75")

// messageIDs mints the ids for the bundles a run records.
//
// The loop runs inside a workflow under the durable runtimes, so a uuid drawn
// on the spot would differ on every replay. Both runtimes offer a way to make
// one draw repeatable — a Temporal side effect, restate's seeded source — but
// each writes to the journal, and the loop mints an id per message.
//
// Nothing needs to be recorded. Replay re-executes the loop in the same order
// it ran the first time, which is what DurableStep already depends on, so "the
// nth id of this turn" is a value both passes agree on without being told. The
// ids are version-5 uuids over the turn and that counter: real uuids, derived
// rather than drawn.
//
// The seed is the run and the turn together. The run alone is not enough — a
// run that pauses for an approval and continues keeps its id, and the resumed
// execution would start counting from zero again and mint ids the first
// segment already used.
type messageIDs struct {
	seed string

	mu sync.Mutex
	n  uint64
}

func newMessageIDs(runID, turnID string) *messageIDs {
	return &messageIDs{seed: runID + ":" + turnID}
}

// Next returns the next id for this turn.
func (m *messageIDs) Next() string {
	m.mu.Lock()
	n := m.n
	m.n++
	m.mu.Unlock()

	return uuid.NewSHA1(messageIDNamespace, []byte(m.seed+":"+strconv.FormatUint(n, 10))).String()
}
