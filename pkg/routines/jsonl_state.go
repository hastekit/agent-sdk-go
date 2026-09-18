package routines

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// JSONLStateStore persists local scheduler state in a separate append-only file.
// Open holds an exclusive process lock for this object's lifetime. Acquire also
// prevents two schedulers sharing this object from running together.
type JSONLStateStore struct {
	mu         sync.Mutex
	file       *os.File
	rows       map[string]StateRecord
	owner      uint64
	generation uint64
}

type stateJournalRecord struct {
	// Format distinguishes this file from a routine-definition journal, even
	// deletion records. Opening a definitions file as a state file fails closed.
	Format  string       `json:"format"`
	Record  *StateRecord `json:"record,omitempty"`
	Deleted *StateKey    `json:"deleted,omitempty"`
}

const stateJournalFormat = "hastekit.routines.state.v1"

// OpenJSONLStateStore opens scheduler state. Use a path different from the one
// passed to OpenJSONLStore. Complete lines are durable records; a torn final line
// is truncated on replay, while malformed complete records fail closed.
func OpenJSONLStateStore(path string) (_ *JSONLStateStore, err error) {
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
		}
	}()
	if err = lockJournal(f); err != nil {
		return nil, fmt.Errorf("lock scheduler state journal: %w", err)
	}
	s := &JSONLStateStore{file: f, rows: map[string]StateRecord{}}
	reader := bufio.NewReader(f)
	var offset int64
	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr == io.EOF {
			if len(line) > 0 {
				if err = f.Truncate(offset); err != nil {
					return nil, err
				}
				if err = f.Sync(); err != nil {
					return nil, err
				}
			}
			break
		}
		if readErr != nil {
			return nil, readErr
		}
		var rec stateJournalRecord
		if err = json.Unmarshal(line, &rec); err != nil {
			return nil, fmt.Errorf("scheduler state at byte %d: %w", offset, err)
		}
		if rec.Format != stateJournalFormat {
			return nil, fmt.Errorf("invalid scheduler state format at byte %d; use a separate state file", offset)
		}
		switch {
		case rec.Record != nil && rec.Deleted == nil:
			if err = rec.Record.validate(); err != nil {
				return nil, fmt.Errorf("scheduler state at byte %d: %w", offset, err)
			}
			r := *rec.Record
			s.rows[key(r.Namespace, r.RoutineID)] = r
		case rec.Record == nil && rec.Deleted != nil && rec.Deleted.Namespace != "" && rec.Deleted.RoutineID != "":
			delete(s.rows, key(rec.Deleted.Namespace, rec.Deleted.RoutineID))
		default:
			return nil, fmt.Errorf("invalid scheduler state record at byte %d", offset)
		}
		offset += int64(len(line))
	}
	if _, err = f.Seek(0, io.SeekEnd); err != nil {
		return nil, err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	err = dir.Sync()
	_ = dir.Close()
	if err != nil {
		return nil, err
	}
	return s, nil
}
func (s *JSONLStateStore) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.file == nil {
		return os.ErrClosed
	}
	return nil
}
func (s *JSONLStateStore) Acquire(ctx context.Context) (func() error, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	if s.owner != 0 {
		return nil, ErrSchedulerOwned
	}
	s.generation++
	token := s.generation
	s.owner = token
	return func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.owner == token {
			s.owner = 0
		}
		return nil
	}, nil
}
func (s *JSONLStateStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	if s.owner != 0 {
		return ErrSchedulerOwned
	}
	err := s.file.Close()
	s.file = nil
	return err
}
func (s *JSONLStateStore) List(ctx context.Context) ([]StateRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	out := make([]StateRecord, 0, len(s.rows))
	for _, r := range s.rows {
		r.State = copyState(r.State)
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		return key(out[i].Namespace, out[i].RoutineID) < key(out[j].Namespace, out[j].RoutineID)
	})
	return out, nil
}
func (s *JSONLStateStore) Get(ctx context.Context, ns, id string) (RoutineState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return RoutineState{}, err
	}
	r, ok := s.rows[key(ns, id)]
	if !ok {
		return RoutineState{}, ErrNotFound
	}
	return copyState(r.State), nil
}
func (s *JSONLStateStore) Save(ctx context.Context, r StateRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return err
	}
	if s.owner == 0 {
		return ErrSchedulerLeaseLost
	}
	if err := r.validate(); err != nil {
		return err
	}
	r.State = copyState(r.State)
	if err := s.append(stateJournalRecord{Format: stateJournalFormat, Record: &r}); err != nil {
		return err
	}
	s.rows[key(r.Namespace, r.RoutineID)] = r
	return nil
}
func (s *JSONLStateStore) Delete(ctx context.Context, ns, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return err
	}
	if s.owner == 0 {
		return ErrSchedulerLeaseLost
	}
	if _, ok := s.rows[key(ns, id)]; !ok {
		return nil
	}
	if err := s.append(stateJournalRecord{Format: stateJournalFormat, Deleted: &StateKey{Namespace: ns, RoutineID: id}}); err != nil {
		return err
	}
	delete(s.rows, key(ns, id))
	return nil
}
func (s *JSONLStateStore) Due(ctx context.Context, now time.Time) ([]StateKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	if s.owner == 0 {
		return nil, ErrSchedulerLeaseLost
	}
	due := []StateRecord{}
	for _, r := range s.rows {
		at := stateDueAt(r.State)
		if !at.IsZero() && !at.After(now) {
			due = append(due, r)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		a, b := stateDueAt(due[i].State), stateDueAt(due[j].State)
		if a.Equal(b) {
			return key(due[i].Namespace, due[i].RoutineID) < key(due[j].Namespace, due[j].RoutineID)
		}
		return a.Before(b)
	})
	out := make([]StateKey, 0, len(due))
	for _, r := range due {
		out = append(out, r.StateKey)
	}
	return out, nil
}
func (s *JSONLStateStore) append(rec stateJournalRecord) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	offset, err := s.file.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	_, err = io.Copy(s.file, bytes.NewReader(b))
	if err == nil {
		err = s.file.Sync()
	}
	if err != nil {
		rollback := s.file.Truncate(offset)
		if rollback == nil {
			rollback = s.file.Sync()
		}
		if rollback != nil {
			_ = s.file.Close()
			s.file = nil
			return errors.Join(err, rollback)
		}
	}
	return err
}

var _ StateStore = (*JSONLStateStore)(nil)
