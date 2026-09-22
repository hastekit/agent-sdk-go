package routines

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// ErrSchedulerOwned means another scheduler already owns the state backend.
var ErrSchedulerOwned = errors.New("scheduler backend is owned by another scheduler")

// ErrSchedulerNotRunning means this scheduler cannot accept manual runs.
var ErrSchedulerNotRunning = errors.New("scheduler is not running in this process")

// ErrSchedulerLeaseLost means a scheduler no longer owns the state backend.
var ErrSchedulerLeaseLost = errors.New("scheduler backend ownership lost")

// Scheduler owns timing, retries, and execution state. Definitions remain in
// Service's Store. Run reconciles definition changes and blocks until shutdown.
// Use only one running scheduler per definition store, including when switching
// backend. Stop and await Run before closing dependencies.
type Scheduler interface {
	Run(context.Context) error
	Status(context.Context, string, string) (RoutineState, error)
	// RunNow queues an enabled routine without changing its scheduled occurrence.
	RunNow(context.Context, string, string) (Run, error)
}

type SchedulerConfig struct {
	PollInterval time.Duration // default one second; also definition reconciliation interval
	Concurrency  int           // default four
	RunTimeout   time.Duration // default ten minutes; executors must honor cancellation
	RetryDelay   time.Duration // default one minute
	MaxAttempts  int           // default three
}

func (c SchedulerConfig) defaults() SchedulerConfig {
	if c.PollInterval <= 0 {
		c.PollInterval = time.Second
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 4
	}
	if c.RunTimeout <= 0 {
		c.RunTimeout = 10 * time.Minute
	}
	if c.RetryDelay <= 0 {
		c.RetryDelay = time.Minute
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	return c
}

type Run struct {
	Manual      bool       `json:"manual,omitempty"`
	ID          string     `json:"id"`
	AgentRunID  string     `json:"agent_run_id"`
	ScheduledAt time.Time  `json:"scheduled_at"`
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	RetryAt     time.Time  `json:"retry_at"`
	Attempts    int        `json:"attempts"`
	Status      string     `json:"status"`
	Error       string     `json:"error,omitempty"`
}

type RoutineState struct {
	DefinitionVersion uint64    `json:"definition_version"`
	NextRunAt         time.Time `json:"next_run_at"`
	Paused            bool      `json:"paused"`
	Pending           *Run      `json:"pending,omitempty"`
	LastRun           *Run      `json:"last_run,omitempty"`
}

type scheduledRoutine struct {
	Routine Routine      `json:"routine"`
	State   RoutineState `json:"state"`
}

func copyState(s RoutineState) RoutineState {
	cp := func(r *Run) *Run {
		if r == nil {
			return nil
		}
		v := *r
		if r.FinishedAt != nil {
			t := *r.FinishedAt
			v.FinishedAt = &t
		}
		return &v
	}
	s.Pending = cp(s.Pending)
	s.LastRun = cp(s.LastRun)
	return s
}
func (r scheduledRoutine) dueAt() time.Time {
	if !r.Routine.Enabled || r.State.Paused {
		return time.Time{}
	}
	if r.State.Pending != nil {
		return r.State.Pending.RetryAt
	}
	return r.State.NextRunAt
}
func newScheduled(r Routine, now time.Time) (scheduledRoutine, error) {
	v := scheduledRoutine{Routine: r, State: RoutineState{DefinitionVersion: r.Version}}
	if r.Enabled {
		next, err := r.Schedule.next(now)
		if err != nil {
			return v, err
		}
		v.State.NextRunAt = next
	}
	return v, nil
}

type stateBackend interface {
	Load(context.Context) (map[string]scheduledRoutine, error)
	Save(context.Context, string, scheduledRoutine) error
	Delete(context.Context, string) error
	Due(context.Context, time.Time) ([]string, error)
}

type manualRequest struct {
	ctx    context.Context
	ns, id string
	reply  chan manualResult
}
type manualResult struct {
	run Run
	err error
}

type schedulerEngine struct {
	manualMu sync.RWMutex
	manual   chan manualRequest
	stopped  chan struct{}
	service  *Service
	config   SchedulerConfig
	backend  stateBackend
}

func (e *schedulerEngine) status(ctx context.Context, ns, id string) (RoutineState, error) {
	rows, err := e.backend.Load(ctx)
	if err != nil {
		return RoutineState{}, err
	}
	r, ok := rows[key(namespace(ns), id)]
	if !ok {
		return RoutineState{}, ErrNotFound
	}
	return copyState(r.State), nil
}

// runNow hands the write to the active scheduler so it is serialized with
// dispatch and completion, and committed before the caller receives acceptance.
func (e *schedulerEngine) runNow(ctx context.Context, ns, id string) (Run, error) {
	e.manualMu.RLock()
	requests, stopped := e.manual, e.stopped
	e.manualMu.RUnlock()
	if requests == nil {
		return Run{}, ErrSchedulerNotRunning
	}
	req := manualRequest{ctx: ctx, ns: namespace(ns), id: id, reply: make(chan manualResult, 1)}
	select {
	case requests <- req:
	case <-stopped:
		return Run{}, ErrSchedulerNotRunning
	case <-ctx.Done():
		return Run{}, ctx.Err()
	}
	select {
	case result := <-req.reply:
		return result.run, result.err
	case <-stopped:
		return Run{}, ErrSchedulerNotRunning
	case <-ctx.Done():
		return Run{}, ctx.Err()
	}
}

// run is shared by local and Redis. Every state transition commits to the
// scheduler backend; the definition Store is never written by a scheduler.
func (e *schedulerEngine) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()
	rows, err := e.backend.Load(ctx)
	if err != nil {
		return err
	}
	active := map[string]context.CancelFunc{}
	type completion struct {
		k         string
		version   uint64
		err       error
		cancelled bool
	}
	done := make(chan completion, e.config.Concurrency)
	reconcile := func() error {
		definitions, err := e.service.store.List(ctx, "")
		if err != nil {
			return err
		}
		present := map[string]bool{}
		for _, r := range definitions {
			k := key(r.Namespace, r.ID)
			present[k] = true
			if old, ok := rows[k]; ok && old.Routine.Version == r.Version {
				continue
			}
			if stop := active[k]; stop != nil {
				stop()
				continue
			}
			next, err := newScheduled(r, time.Now().UTC())
			if err != nil {
				return err
			}
			if err = e.backend.Save(ctx, k, next); err != nil {
				return err
			}
			rows[k] = next
		}
		for k := range rows {
			if !present[k] {
				if stop := active[k]; stop != nil {
					stop()
					continue
				}
				if err := e.backend.Delete(ctx, k); err != nil {
					return err
				}
				delete(rows, k)
			}
		}
		return nil
	}
	dispatch := func() error {
		keys, err := e.backend.Due(ctx, time.Now().UTC())
		if err != nil {
			return err
		}
		for _, k := range keys {
			if len(active) >= e.config.Concurrency {
				break
			}
			if active[k] != nil {
				continue
			}
			r, ok := rows[k]
			if !ok {
				continue
			}
			// Recheck definitions immediately before dispatch. A concurrent edit can
			// still race execution; the next reconciliation cancels that older run.
			current, err := e.service.store.Get(ctx, r.Routine.Namespace, r.Routine.ID)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if current.Version != r.Routine.Version || !current.Enabled {
				continue
			}
			now := time.Now().UTC()
			if r.dueAt().IsZero() || r.dueAt().After(now) {
				continue
			}
			r.State = copyState(r.State)
			if r.State.Pending == nil {
				r.State.Pending = &Run{ID: uuid.NewString(), ScheduledAt: r.State.NextRunAt, RetryAt: now}
			}
			exhausted := r.State.Pending.Attempts >= e.config.MaxAttempts
			if !exhausted {
				r.State.Pending.Attempts++
				r.State.Pending.AgentRunID = uuid.NewString()
				r.State.Pending.StartedAt = now
			}
			r.State.Pending.Status = "running"
			if err = e.backend.Save(ctx, k, r); err != nil {
				return err
			}
			rows[k] = r
			runCtx, stop := context.WithCancel(ctx)
			active[k] = stop
			wg.Add(1)
			go func(k string, r scheduledRoutine) {
				defer wg.Done()
				defer stop()
				err := executeAttempt(runCtx, e.service.executor, r.Routine, *r.State.Pending, e.config, exhausted)
				done <- completion{k, r.Routine.Version, err, runCtx.Err() != nil}
			}(k, r)
		}
		return nil
	}
	tick := func() error {
		if err := reconcile(); err != nil {
			return err
		}
		return dispatch()
	}
	if err := tick(); err != nil {
		return err
	}
	requests := make(chan manualRequest)
	stopped := make(chan struct{})
	e.manualMu.Lock()
	e.manual, e.stopped = requests, stopped
	e.manualMu.Unlock()
	defer func() {
		e.manualMu.Lock()
		e.manual = nil
		close(stopped)
		e.manualMu.Unlock()
	}()
	enqueue := func(req manualRequest) (Run, error) {
		if err := req.ctx.Err(); err != nil {
			return Run{}, err
		}
		definition, err := e.service.Get(req.ctx, req.ns, req.id)
		if err != nil {
			return Run{}, err
		}
		if !definition.Enabled {
			return Run{}, fmt.Errorf("%w: resume the routine before running it", ErrConflict)
		}
		k := key(req.ns, req.id)
		r, ok := rows[k]
		if active[k] != nil || (ok && r.State.Pending != nil) {
			return Run{}, fmt.Errorf("%w: routine already has a pending run", ErrConflict)
		}
		if !ok || r.Routine.Version != definition.Version {
			r, err = newScheduled(definition, time.Now().UTC())
			if err != nil {
				return Run{}, err
			}
		}
		if r.State.Paused {
			return Run{}, fmt.Errorf("%w: routine is waiting for input", ErrConflict)
		}
		now := time.Now().UTC()
		run := Run{ID: uuid.NewString(), Manual: true, ScheduledAt: now, RetryAt: now, Status: "queued"}
		r.State = copyState(r.State)
		r.State.Pending = &run
		if err := e.backend.Save(ctx, k, r); err != nil {
			return Run{}, err
		}
		rows[k] = r
		return run, nil
	}
	timer := time.NewTicker(e.config.PollInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case req := <-requests:
			run, err := enqueue(req)
			req.reply <- manualResult{run, err}
			if err == nil {
				if err := dispatch(); err != nil {
					return err
				}
			}
		case c := <-done:
			delete(active, c.k)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !c.cancelled {
				r := rows[c.k]
				if r.Routine.Version == c.version {
					r, err = finishAttempt(r, c.err, time.Now().UTC(), e.config)
					if err != nil {
						return err
					}
					if err = e.backend.Save(ctx, c.k, r); err != nil {
						return err
					}
					rows[c.k] = r
				}
			}
			if err := tick(); err != nil {
				return err
			}
		case <-timer.C:
			if err := tick(); err != nil {
				return err
			}
		}
	}
}
func executeAttempt(ctx context.Context, executor Executor, r Routine, run Run, config SchedulerConfig, exhausted bool) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("executor panic: %v", p)
		}
	}()
	if exhausted {
		return errors.New("execution interrupted; maximum attempts exhausted")
	}
	ctx, cancel := context.WithTimeout(ctx, config.RunTimeout)
	defer cancel()
	err = executor.Execute(ctx, r, run)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
func finishAttempt(r scheduledRoutine, err error, now time.Time, c SchedulerConfig) (scheduledRoutine, error) {
	r.State = copyState(r.State)
	run := r.State.Pending
	if err != nil && !errors.Is(err, ErrPaused) && run.Attempts < c.MaxAttempts {
		run.Status = "retrying"
		run.Error = err.Error()
		run.RetryAt = now.Add(c.RetryDelay)
		return r, nil
	}
	run.Status = "succeeded"
	run.Error = ""
	run.FinishedAt = &now
	if err != nil {
		run.Status = "failed"
		run.Error = err.Error()
	}
	r.State.LastRun = run
	r.State.Pending = nil
	if run.Manual {
		if errors.Is(err, ErrPaused) {
			run.Status = "paused"
		}
		return r, nil
	}
	r.State.NextRunAt = time.Time{}
	if errors.Is(err, ErrPaused) {
		run.Status = "paused"
		r.State.Paused = true
	} else if r.Routine.Schedule.At == nil {
		next, nextErr := r.Routine.Schedule.next(now)
		if nextErr != nil {
			return r, nextErr
		}
		r.State.NextRunAt = next
	}
	return r, nil
}

// LocalScheduler executes in this process and persists all scheduling state in
// the supplied StateStore. A separate JSONLStateStore is provided; applications
// can implement StateStore for other persistent backends.
type LocalScheduler struct {
	engine     schedulerEngine
	stateStore StateStore
	running    atomic.Bool
}

// NewLocalScheduler requires an explicit state store. Use a different file from
// the routine-definition JSONL. No in-memory or implicit-path fallback is used.
func NewLocalScheduler(service *Service, stateStore StateStore, config SchedulerConfig) *LocalScheduler {
	return &LocalScheduler{engine: schedulerEngine{service: service, config: config.defaults(), backend: &localStateBackend{store: stateStore, definitions: service.store}}, stateStore: stateStore}
}

// NewScheduler is the compatibility name for the local implementation.
func NewScheduler(service *Service, stateStore StateStore, config SchedulerConfig) *LocalScheduler {
	return NewLocalScheduler(service, stateStore, config)
}
func (s *LocalScheduler) Run(ctx context.Context) (err error) {
	if s.stateStore == nil {
		return errors.New("local scheduler requires a persistent state store")
	}
	if !s.running.CompareAndSwap(false, true) {
		return errors.New("scheduler already running")
	}
	defer s.running.Store(false)
	if !s.engine.service.running.CompareAndSwap(false, true) {
		return errors.New("service already has a running scheduler")
	}
	defer s.engine.service.running.Store(false)
	release, err := s.stateStore.Acquire(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, release()) }()
	return s.engine.run(ctx)
}
func (s *LocalScheduler) Status(ctx context.Context, ns, id string) (RoutineState, error) {
	if s.stateStore == nil {
		return RoutineState{}, errors.New("local scheduler requires a persistent state store")
	}
	return s.stateStore.Get(ctx, namespace(ns), id)
}

var _ Scheduler = (*LocalScheduler)(nil)

func (s *LocalScheduler) RunNow(ctx context.Context, ns, id string) (Run, error) {
	return s.engine.runNow(ctx, ns, id)
}
