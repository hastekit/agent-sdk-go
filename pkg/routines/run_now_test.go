package routines

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func testRunNow(t *testing.T, makeScheduler func(*Service) Scheduler) {
	t.Helper()
	ctx := context.Background()
	release := make(chan struct{})
	defer close(release)
	started := make(chan Run, 1)
	service := NewService(testStore(t), &fakeExecutor{execute: func(ctx context.Context, _ Routine, run Run) error {
		started <- run
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}})
	routine, err := service.Create(ctx, "tenant", definition())
	if err != nil {
		t.Fatal(err)
	}
	scheduler := makeScheduler(service)
	handler := NewHTTPHandler(service, HTTPConfig{Scheduler: scheduler, NamespaceResolver: func(r *http.Request) (string, error) { return r.Header.Get("X-Tenant"), nil }})
	request := func(ns string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/routines/"+routine.ID+"/run", nil)
		req.Header.Set("X-Tenant", ns)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	if res := request("tenant"); res.Code != 503 {
		t.Fatalf("stopped scheduler: %d %s", res.Code, res.Body.String())
	}
	stop := startScheduler(t, scheduler)
	defer stop()
	await(t, func() bool { _, err := scheduler.Status(ctx, routine.Namespace, routine.ID); return err == nil })
	before := state(t, scheduler, routine)
	if res := request("other"); res.Code != 404 {
		t.Fatalf("tenant isolation: %d", res.Code)
	}
	var res *httptest.ResponseRecorder
	await(t, func() bool { res = request("tenant"); return res.Code != 503 })
	if res.Code != 202 {
		t.Fatalf("queue: %d %s", res.Code, res.Body.String())
	}
	var queued Run
	if err := json.Unmarshal(res.Body.Bytes(), &queued); err != nil {
		t.Fatal(err)
	}
	if !queued.Manual || queued.ID == "" || queued.Status != "queued" {
		t.Fatalf("invalid acceptance: %+v", queued)
	}
	select {
	case run := <-started:
		if run.ID != queued.ID || !run.Manual || run.AgentRunID == "" {
			t.Fatalf("wrong dispatch: %+v", run)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("manual run was not dispatched")
	}
	if res := request("tenant"); res.Code != 409 {
		t.Fatalf("duplicate pending run: %d", res.Code)
	}
	release <- struct{}{}
	await(t, func() bool { return completed(scheduler, routine) })
	after := state(t, scheduler, routine)
	if !after.NextRunAt.Equal(before.NextRunAt) || after.LastRun.ID != queued.ID {
		t.Fatalf("schedule changed: before=%+v after=%+v", before, after)
	}
	saved, err := service.Get(ctx, routine.Namespace, routine.ID)
	if err != nil || saved.Version != routine.Version {
		t.Fatal("manual run changed definition", saved, err)
	}
	if _, err := service.SetEnabled(ctx, routine.Namespace, routine.ID, false); err != nil {
		t.Fatal(err)
	}
	if res := request("tenant"); res.Code != 409 {
		t.Fatalf("paused routine accepted: %d", res.Code)
	}
}

func TestLocalRunNowHTTP(t *testing.T) {
	testRunNow(t, func(service *Service) Scheduler {
		return NewLocalScheduler(service, testStateStore(t), SchedulerConfig{PollInterval: time.Millisecond})
	})
}
func TestRedisRunNowHTTP(t *testing.T) {
	client, prefix := redisTestClient(t)
	testRunNow(t, func(service *Service) Scheduler {
		return NewRedisScheduler(service, client, RedisSchedulerConfig{Prefix: prefix, SchedulerConfig: SchedulerConfig{PollInterval: time.Millisecond}})
	})
}

func TestManualCompletionPreservesSchedule(t *testing.T) {
	for _, result := range []error{nil, errors.New("failed"), ErrPaused} {
		now := time.Now().UTC()
		next := now.Add(time.Hour)
		r := scheduledRoutine{Routine: Routine{Definition: Definition{Schedule: Schedule{At: &next}}}, State: RoutineState{NextRunAt: next, Pending: &Run{Manual: true, Attempts: 1}}}
		finished, err := finishAttempt(r, result, now, SchedulerConfig{MaxAttempts: 1})
		if err != nil || !finished.State.NextRunAt.Equal(next) || finished.State.Paused || finished.State.Pending != nil {
			t.Fatalf("manual completion affected scheduled execution: %+v, %v", finished, err)
		}
	}
}

func TestManualRunRecoversAfterRestart(t *testing.T) {
	ctx := context.Background()
	store := testStateStore(t)
	started := make(chan Run, 1)
	service := NewService(testStore(t), &fakeExecutor{execute: func(ctx context.Context, _ Routine, run Run) error {
		started <- run
		<-ctx.Done()
		return ctx.Err()
	}})
	routine, err := service.Create(ctx, "tenant", definition())
	if err != nil {
		t.Fatal(err)
	}
	config := SchedulerConfig{PollInterval: time.Millisecond}
	first := NewLocalScheduler(service, store, config)
	stop := startScheduler(t, first)
	var queued Run
	await(t, func() bool {
		queued, err = first.RunNow(ctx, routine.Namespace, routine.ID)
		return !errors.Is(err, ErrSchedulerNotRunning)
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("run not started")
	}
	next := state(t, first, routine).NextRunAt
	stop()
	recovered := make(chan Run, 1)
	service.executor = &fakeExecutor{execute: func(_ context.Context, _ Routine, run Run) error { recovered <- run; return nil }}
	second := NewLocalScheduler(service, store, config)
	startScheduler(t, second)
	select {
	case run := <-recovered:
		if !run.Manual || run.ID != queued.ID || run.Attempts != 2 {
			t.Fatalf("wrong recovered run: %+v", run)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("manual run not recovered")
	}
	await(t, func() bool { return completed(second, routine) })
	if !state(t, second, routine).NextRunAt.Equal(next) {
		t.Fatal("restart changed schedule")
	}
}
