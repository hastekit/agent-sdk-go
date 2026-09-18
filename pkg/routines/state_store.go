package routines

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// StateKey identifies a routine without copying its definition into state storage.
type StateKey struct {
	Namespace string `json:"namespace"`
	RoutineID string `json:"routine_id"`
}

// StateRecord contains scheduler-owned state only. DefinitionVersion identifies
// the definition revision this state belongs to; definitions remain in Store.
type StateRecord struct {
	StateKey
	State RoutineState `json:"state"`
}

// StateStore is the pluggable persistence contract for LocalScheduler.
//
// Acquire must grant exclusive ownership across scheduler instances/processes
// using this store, or return ErrSchedulerOwned. Its release function is called
// after all executions and writes stop. Save/Delete/Due require ownership. Writes must
// durably commit before returning nil; failed writes must not be visible as
// successful state transitions. Implementations must be concurrency-safe and
// return independent copies of records. List/Get may be used without ownership.
// Due returns records with a nonzero due time <= now, ordered by due time; for
// pending runs use RetryAt, otherwise NextRunAt. Paused records are not due.
//
// The caller owns the store's lifetime: stop and await Run before closing it.
// This interface permits database implementations without depending on a driver.
type StateStore interface {
	Acquire(context.Context) (release func() error, err error)
	List(context.Context) ([]StateRecord, error)
	Get(context.Context, string, string) (RoutineState, error)
	Save(context.Context, StateRecord) error
	Delete(context.Context, string, string) error
	Due(context.Context, time.Time) ([]StateKey, error)
}

func (r StateRecord) validate() error {
	if r.Namespace == "" || r.RoutineID == "" || strings.ContainsRune(r.RoutineID, 0) || r.State.DefinitionVersion == 0 {
		return fmt.Errorf("%w: state requires namespace, routine ID and definition version", ErrInvalid)
	}
	return nil
}
func stateDueAt(s RoutineState) time.Time {
	if s.Paused {
		return time.Time{}
	}
	if s.Pending != nil {
		return s.Pending.RetryAt
	}
	return s.NextRunAt
}

// localStateBackend joins definitions only in memory. The state store never
// receives agent names, instructions, or schedule definitions.
type localStateBackend struct {
	store       StateStore
	definitions Store
}

func (b *localStateBackend) Load(ctx context.Context) (map[string]scheduledRoutine, error) {
	states, err := b.store.List(ctx)
	if err != nil {
		return nil, err
	}
	definitions, err := b.definitions.List(ctx, "")
	if err != nil {
		return nil, err
	}
	byID := map[string]Routine{}
	for _, r := range definitions {
		byID[key(r.Namespace, r.ID)] = r
	}
	out := map[string]scheduledRoutine{}
	for _, rec := range states {
		if err := rec.validate(); err != nil {
			return nil, err
		}
		k := key(rec.Namespace, rec.RoutineID)
		r, ok := byID[k]
		if !ok {
			r = Routine{Namespace: rec.Namespace, ID: rec.RoutineID}
		}
		// Retain the saved version so reconciliation detects definition edits made
		// while this scheduler was offline, including deleted definitions.
		r.Version = rec.State.DefinitionVersion
		out[k] = scheduledRoutine{Routine: r, State: copyState(rec.State)}
	}
	return out, nil
}
func (b *localStateBackend) Save(ctx context.Context, _ string, r scheduledRoutine) error {
	return b.store.Save(ctx, StateRecord{StateKey: StateKey{Namespace: r.Routine.Namespace, RoutineID: r.Routine.ID}, State: copyState(r.State)})
}
func (b *localStateBackend) Delete(ctx context.Context, k string) error {
	i := strings.LastIndexByte(k, 0)
	if i < 0 {
		return ErrInvalid
	}
	return b.store.Delete(ctx, k[:i], k[i+1:])
}
func (b *localStateBackend) Due(ctx context.Context, now time.Time) ([]string, error) {
	refs, err := b.store.Due(ctx, now)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		out = append(out, key(ref.Namespace, ref.RoutineID))
	}
	return out, nil
}
