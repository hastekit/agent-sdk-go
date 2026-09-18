// Package routines provides persistent scheduled agent tasks. Run one Scheduler
// per definition store. Scheduler implementations own execution state.
package routines

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"
)

var (
	ErrNotFound = errors.New("routine not found")
	ErrConflict = errors.New("routine changed concurrently")
	ErrInvalid  = errors.New("invalid routine")
)

type Schedule struct {
	At       *time.Time `json:"at,omitempty" jsonschema_description:"One-time RFC3339 datetime including UTC offset; mutually exclusive with cron"`
	Cron     string     `json:"cron,omitempty" jsonschema_description:"Five-field cron expression: minute hour day-of-month month day-of-week; mutually exclusive with at"`
	Timezone string     `json:"timezone,omitempty" jsonschema_description:"IANA time zone for cron, defaults to UTC"`
}

func (s Schedule) next(after time.Time) (time.Time, error) {
	if (s.At != nil) == (strings.TrimSpace(s.Cron) != "") {
		return time.Time{}, fmt.Errorf("%w: specify exactly one of at or cron", ErrInvalid)
	}
	if s.At != nil {
		if s.At.IsZero() || s.Timezone != "" {
			return time.Time{}, fmt.Errorf("%w: at must be a datetime with offset; timezone is only for cron", ErrInvalid)
		}
		return s.At.UTC(), nil
	}
	if len(strings.Fields(s.Cron)) != 5 {
		return time.Time{}, fmt.Errorf("%w: cron requires five fields", ErrInvalid)
	}
	zone := s.Timezone
	if zone == "" {
		zone = "UTC"
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return time.Time{}, fmt.Errorf("%w: unknown timezone", ErrInvalid)
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	spec, err := parser.Parse("CRON_TZ=" + zone + " " + s.Cron)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	next := spec.Next(after).UTC()
	if next.IsZero() {
		return time.Time{}, fmt.Errorf("%w: cron has no future occurrence", ErrInvalid)
	}
	return next, nil
}

type Definition struct {
	Name        string   `json:"name" jsonschema_description:"Human-readable routine name"`
	Agent       string   `json:"agent" jsonschema_description:"Registered target agent name"`
	Instruction string   `json:"instruction" jsonschema_description:"User message to send on each occurrence"`
	Schedule    Schedule `json:"schedule"`
}

// Routine contains configuration only. Execution state belongs to a Scheduler.
type Routine struct {
	Definition
	ID        string    `json:"id"`
	Namespace string    `json:"namespace"`
	Version   uint64    `json:"version"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store updates must atomically compare Version, durably commit, and increment
// Version. Create uses expected=0. List with an empty namespace lists all tenants
// for the scheduler; services must supply a nonempty authorized namespace.
type Store interface {
	List(context.Context, string) ([]Routine, error)
	Get(context.Context, string, string) (Routine, error)
	Save(context.Context, Routine, uint64) (Routine, error)
	Delete(context.Context, string, string, uint64) error
}

type Executor interface {
	HasAgent(string) bool
	Execute(context.Context, Routine, Run) error
}

type Service struct {
	store    Store
	executor Executor
	running  atomic.Bool
}

func NewService(store Store, executor Executor) *Service {
	return &Service{store: store, executor: executor}
}
func namespace(ns string) string {
	if ns == "" {
		return "default"
	}
	return ns
}
func (s *Service) validate(d Definition, now time.Time) (time.Time, error) {
	if strings.TrimSpace(d.Name) == "" || strings.TrimSpace(d.Instruction) == "" || !s.executor.HasAgent(d.Agent) {
		return time.Time{}, fmt.Errorf("%w: name, instruction and an available agent are required", ErrInvalid)
	}
	next, err := d.Schedule.next(now)
	if err == nil && !next.After(now) {
		err = fmt.Errorf("%w: datetime must be in the future", ErrInvalid)
	}
	return next, err
}
func (s *Service) Create(ctx context.Context, ns string, d Definition) (Routine, error) {
	now := time.Now().UTC()
	_, err := s.validate(d, now)
	if err != nil {
		return Routine{}, err
	}
	return s.store.Save(ctx, Routine{Definition: d, ID: uuid.NewString(), Namespace: namespace(ns), Enabled: true, CreatedAt: now, UpdatedAt: now}, 0)
}
func (s *Service) List(ctx context.Context, ns string) ([]Routine, error) {
	return s.store.List(ctx, namespace(ns))
}
func (s *Service) Get(ctx context.Context, ns, id string) (Routine, error) {
	return s.store.Get(ctx, namespace(ns), id)
}
func (s *Service) Update(ctx context.Context, ns, id string, d Definition) (Routine, error) {
	r, err := s.Get(ctx, ns, id)
	if err != nil {
		return r, err
	}
	now := time.Now().UTC()
	_, err = s.validate(d, now)
	if err != nil {
		return r, err
	}
	r.Definition = d
	r.UpdatedAt = now
	return s.store.Save(ctx, r, r.Version)
}
func (s *Service) SetEnabled(ctx context.Context, ns, id string, enabled bool) (Routine, error) {
	r, err := s.Get(ctx, ns, id)
	if err != nil {
		return r, err
	}
	if enabled {
		_, err := s.validate(r.Definition, time.Now())
		if err != nil {
			return r, err
		}
	}
	r.Enabled = enabled
	r.UpdatedAt = time.Now().UTC()
	return s.store.Save(ctx, r, r.Version)
}
func (s *Service) Delete(ctx context.Context, ns, id string) error {
	r, err := s.Get(ctx, ns, id)
	if err != nil {
		return err
	}
	return s.store.Delete(ctx, r.Namespace, r.ID, r.Version)
}
