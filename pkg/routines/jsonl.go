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
)

// JSONLStore is a single-process append-only journal. Open obtains an exclusive
// advisory lock; keep one instance shared by the service and scheduler. Files
// must reside on a local persistent filesystem supporting flock and fsync.
type JSONLStore struct {
	mu   sync.Mutex
	file *os.File
	rows map[string]Routine
}
type record struct {
	Routine   *Routine `json:"routine,omitempty"`
	Namespace string   `json:"namespace,omitempty"`
	Deleted   string   `json:"deleted,omitempty"`
}

func key(ns, id string) string { return ns + "\x00" + id }
func clone(r Routine) Routine {
	if r.Schedule.At != nil {
		t := *r.Schedule.At
		r.Schedule.At = &t
	}
	return r
}

// OpenJSONLStore replays complete lines. A final unterminated line is an
// uncommitted/torn append and is truncated; corruption in complete lines fails
// closed rather than silently losing scheduled work.
func OpenJSONLStore(path string) (_ *JSONLStore, err error) {
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
		return nil, fmt.Errorf("lock routines journal: %w", err)
	}
	s := &JSONLStore{file: f, rows: map[string]Routine{}}
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
		var rec record
		if err = json.Unmarshal(line, &rec); err != nil {
			return nil, fmt.Errorf("routines journal at byte %d: %w", offset, err)
		}
		switch {
		case rec.Routine != nil && rec.Deleted == "":
			r := rec.Routine
			if r.ID == "" || r.Namespace == "" || r.Version == 0 {
				return nil, fmt.Errorf("invalid routine record at byte %d", offset)
			}
			s.rows[key(r.Namespace, r.ID)] = *r
		case rec.Routine == nil && rec.Deleted != "" && rec.Namespace != "":
			delete(s.rows, key(rec.Namespace, rec.Deleted))
		default:
			return nil, fmt.Errorf("invalid journal record at byte %d", offset)
		}
		offset += int64(len(line))
	}
	if _, err = f.Seek(0, io.SeekEnd); err != nil {
		return nil, err
	}
	// Persist the directory entry as well as subsequent journal appends.
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
func (s *JSONLStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}
func (s *JSONLStore) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.file == nil {
		return os.ErrClosed
	}
	return nil
}
func (s *JSONLStore) List(ctx context.Context, ns string) ([]Routine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	out := []Routine{}
	for _, r := range s.rows {
		if ns == "" || r.Namespace == ns {
			out = append(out, clone(r))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func (s *JSONLStore) Get(ctx context.Context, ns, id string) (Routine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return Routine{}, err
	}
	r, ok := s.rows[key(ns, id)]
	if !ok {
		return Routine{}, ErrNotFound
	}
	return clone(r), nil
}
func (s *JSONLStore) append(rec record) error {
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
		// Restore the last committed boundary. If rollback fails, poison the store:
		// appending beyond a torn record would make later recovery unsafe.
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
func (s *JSONLStore) Save(ctx context.Context, r Routine, expected uint64) (Routine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return Routine{}, err
	}
	if r.ID == "" || r.Namespace == "" {
		return Routine{}, ErrInvalid
	}
	old, ok := s.rows[key(r.Namespace, r.ID)]
	if (expected == 0 && ok) || (expected != 0 && (!ok || old.Version != expected)) {
		return Routine{}, ErrConflict
	}
	r = clone(r)
	r.Version = expected + 1
	if err := s.append(record{Routine: &r}); err != nil {
		return Routine{}, err
	}
	s.rows[key(r.Namespace, r.ID)] = r
	return clone(r), nil
}
func (s *JSONLStore) Delete(ctx context.Context, ns, id string, expected uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return err
	}
	old, ok := s.rows[key(ns, id)]
	if !ok {
		return ErrNotFound
	}
	if old.Version != expected {
		return ErrConflict
	}
	if err := s.append(record{Namespace: ns, Deleted: id}); err != nil {
		return err
	}
	delete(s.rows, key(ns, id))
	return nil
}
