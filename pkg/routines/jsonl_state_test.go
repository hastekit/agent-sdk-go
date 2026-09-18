package routines

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJSONLStateReplayIsolationDueAndDelete(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.jsonl")
	store, err := OpenJSONLStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	release, err := store.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	records := []StateRecord{
		{StateKey: StateKey{Namespace: "a", RoutineID: "same"}, State: RoutineState{DefinitionVersion: 1, NextRunAt: now.Add(-time.Hour)}},
		{StateKey: StateKey{Namespace: "b", RoutineID: "same"}, State: RoutineState{DefinitionVersion: 2, Pending: &Run{ID: "interrupted", Attempts: 1, Status: "running", RetryAt: now.Add(-time.Minute)}}},
		{StateKey: StateKey{Namespace: "a", RoutineID: "future"}, State: RoutineState{DefinitionVersion: 1, NextRunAt: now.Add(-time.Hour), Pending: &Run{ID: "retry", RetryAt: now.Add(time.Hour)}}},
		{StateKey: StateKey{Namespace: "a", RoutineID: "paused"}, State: RoutineState{DefinitionVersion: 1, Paused: true, NextRunAt: now.Add(-time.Hour)}},
		{StateKey: StateKey{Namespace: "a", RoutineID: "done"}, State: RoutineState{DefinitionVersion: 1, LastRun: &Run{ID: "done", Status: "succeeded", FinishedAt: &now}}},
	}
	for _, r := range records {
		if err := store.Save(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	records[1].State.Pending.ID = "caller mutation"
	value, _ := store.Get(ctx, "b", "same")
	if value.Pending.ID != "interrupted" {
		t.Fatal("write aliases caller")
	}
	value.Pending.ID = "reader mutation"
	value, _ = store.Get(ctx, "b", "same")
	if value.Pending.ID != "interrupted" {
		t.Fatal("read aliases store")
	}
	due, err := store.Due(ctx, now)
	if err != nil || len(due) != 2 || due[0].Namespace != "a" || due[1].Namespace != "b" {
		t.Fatalf("due=%+v err=%v", due, err)
	}
	if _, err := store.Get(ctx, "other", "same"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString(`{"format":`)
	_ = file.Close()
	store, err = OpenJSONLStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	value, err = store.Get(ctx, "b", "same")
	if err != nil || value.Pending.ID != "interrupted" || value.Pending.Attempts != 1 {
		t.Fatalf("%+v %v", value, err)
	}
	value, err = store.Get(ctx, "a", "done")
	if err != nil || value.LastRun.FinishedAt == nil || !value.LastRun.FinishedAt.Equal(now) {
		t.Fatalf("%+v %v", value, err)
	}
	release, err = store.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "a", "same"); err != nil {
		t.Fatal(err)
	}
	_ = release()
	_ = store.Close()
	store, err = OpenJSONLStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Get(ctx, "a", "same"); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted state resurrected", err)
	}
	if _, err := store.Get(ctx, "b", "same"); err != nil {
		t.Fatal("deleted another namespace", err)
	}
}
func TestJSONLStateOwnershipAndSeparateFile(t *testing.T) {
	ctx := context.Background()
	store := testStateStore(t)
	if other, err := OpenJSONLStateStore(store.file.Name()); err == nil {
		_ = other.Close()
		t.Fatal("second process lock accepted")
	}
	rec := StateRecord{StateKey: StateKey{Namespace: "a", RoutineID: "id"}, State: RoutineState{DefinitionVersion: 1}}
	if err := store.Save(ctx, rec); !errors.Is(err, ErrSchedulerLeaseLost) {
		t.Fatal(err)
	}
	first, err := store.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Acquire(ctx); !errors.Is(err, ErrSchedulerOwned) {
		t.Fatal(err)
	}
	if err := store.Close(); !errors.Is(err, ErrSchedulerOwned) {
		t.Fatal("closed active state store", err)
	}
	if err := first(); err != nil {
		t.Fatal(err)
	}
	second, err := store.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = first() // Releasing an old generation must not unlock a newer scheduler.
	if _, err := store.Acquire(ctx); !errors.Is(err, ErrSchedulerOwned) {
		t.Fatal(err)
	}
	if err := store.Save(ctx, rec); err != nil {
		t.Fatal(err)
	}
	_ = second()

	defs := testStore(t)
	due(t, defs)
	path := defs.file.Name()
	before, _ := os.ReadFile(path)
	_ = defs.Close()
	if other, err := OpenJSONLStateStore(path); err == nil {
		_ = other.Close()
		t.Fatal("definition journal accepted as state")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("modified definition journal")
	}
	path = store.file.Name()
	_ = store.Close()
	if other, err := OpenJSONLStore(path); err == nil {
		_ = other.Close()
		t.Fatal("state journal accepted as definitions")
	}
}
func TestJSONLStateRejectsCorruptionAndCancelledWrites(t *testing.T) {
	for _, data := range []string{"{broken}\n", "{}\n", `{"format":"hastekit.routines.state.v1","record":{"namespace":"n","routine_id":"id","state":{}}}` + "\n"} {
		path := filepath.Join(t.TempDir(), "state.jsonl")
		_ = os.WriteFile(path, []byte(data), 0600)
		if s, err := OpenJSONLStateStore(path); err == nil {
			_ = s.Close()
			t.Fatal("invalid complete record accepted")
		}
	}
	s := testStateStore(t)
	release, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Save(ctx, StateRecord{StateKey: StateKey{Namespace: "a", RoutineID: "id"}, State: RoutineState{DefinitionVersion: 1}}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	rows, err := s.List(context.Background())
	if err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
}
func TestLocalJSONLRecoversInterruptedOccurrence(t *testing.T) {
	definitions := testStore(t)
	r := due(t, definitions)
	stateStore := testStateStore(t)
	path := stateStore.file.Name()
	started := make(chan Run, 1)
	first := NewLocalScheduler(NewService(definitions, &fakeExecutor{execute: func(ctx context.Context, _ Routine, run Run) error { started <- run; <-ctx.Done(); return ctx.Err() }}), stateStore, SchedulerConfig{PollInterval: time.Millisecond})
	stop := startScheduler(t, first)
	var original Run
	select {
	case original = <-started:
	case <-time.After(time.Second):
		t.Fatal("execution did not start")
	}
	stop()
	if err := stateStore.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenJSONLStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	recovered := make(chan Run, 1)
	second := NewLocalScheduler(NewService(definitions, &fakeExecutor{execute: func(_ context.Context, _ Routine, run Run) error { recovered <- run; return nil }}), reopened, SchedulerConfig{PollInterval: time.Millisecond})
	startScheduler(t, second)
	await(t, func() bool { return completed(second, r) })
	retry := <-recovered
	if retry.ID != original.ID || retry.Attempts != 2 || retry.AgentRunID == original.AgentRunID {
		t.Fatalf("original=%+v retry=%+v", original, retry)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"instruction"`, `"agent"`, `"schedule"`, `"enabled"`} {
		if strings.Contains(string(data), field) {
			t.Fatalf("state journal copied definition field %s", field)
		}
	}
}
func TestLocalRequiresStateStoreAndExclusiveOwnership(t *testing.T) {
	svc := NewService(testStore(t), &fakeExecutor{})
	if err := NewLocalScheduler(svc, nil, SchedulerConfig{}).Run(context.Background()); err == nil {
		t.Fatal("missing state store accepted")
	}
	store := testStateStore(t)
	first := NewLocalScheduler(svc, store, SchedulerConfig{})
	startScheduler(t, first)
	await(t, func() bool { store.mu.Lock(); defer store.mu.Unlock(); return store.owner != 0 })
	otherService := NewService(testStore(t), &fakeExecutor{})
	if err := NewLocalScheduler(otherService, store, SchedulerConfig{}).Run(context.Background()); !errors.Is(err, ErrSchedulerOwned) {
		t.Fatal("second scheduler sharing state accepted", err)
	}
}
