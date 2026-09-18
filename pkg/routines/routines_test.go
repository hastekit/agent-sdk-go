package routines

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

type fakeExecutor struct {
	execute func(context.Context, Routine, Run) error
}

func (*fakeExecutor) HasAgent(name string) bool { return name == "assistant" }
func (*fakeExecutor) AgentNames() []string      { return []string{"assistant"} }
func (f *fakeExecutor) Execute(ctx context.Context, r Routine, run Run) error {
	if f.execute != nil {
		return f.execute(ctx, r, run)
	}
	return nil
}
func testStore(t *testing.T) *JSONLStore {
	t.Helper()
	s, err := OpenJSONLStore(filepath.Join(t.TempDir(), "routines.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func testStateStore(t *testing.T) *JSONLStateStore {
	t.Helper()
	s, err := OpenJSONLStateStore(filepath.Join(t.TempDir(), "scheduler-state.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}
func definition() Definition {
	return Definition{Name: "Daily summary", Agent: "assistant", Instruction: "Summarize my tasks", Schedule: Schedule{Cron: "0 9 * * *", Timezone: "Asia/Kolkata"}}
}
func due(t *testing.T, s Store) Routine {
	t.Helper()
	at := time.Now().Add(time.Hour)
	d := definition()
	d.Schedule = Schedule{At: &at}
	r, err := NewService(s, &fakeExecutor{}).Create(context.Background(), "tenant", d)
	if err != nil {
		t.Fatal(err)
	}
	// Model an overdue persisted definition loaded after downtime.
	at = time.Now().Add(-time.Hour)
	r.Schedule.At = &at
	r, err = s.Save(context.Background(), r, r.Version)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func await(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out")
}
func startScheduler(t *testing.T, s Scheduler) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Errorf("scheduler: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("scheduler did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}
func state(t *testing.T, s Scheduler, r Routine) RoutineState {
	t.Helper()
	v, err := s.Status(context.Background(), r.Namespace, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func completed(s Scheduler, r Routine) bool {
	v, err := s.Status(context.Background(), r.Namespace, r.ID)
	return err == nil && v.LastRun != nil
}

func TestScheduleValidationAndTimezone(t *testing.T) {
	now := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	next, err := definition().Schedule.next(now)
	if err != nil || !next.Equal(time.Date(2026, 9, 18, 3, 30, 0, 0, time.UTC)) {
		t.Fatalf("next=%s err=%v", next, err)
	}
	for _, s := range []Schedule{{}, {At: &now, Cron: "* * * * *"}, {Cron: "* * * * * *"}, {Cron: "0 0 31 2 *"}, {Cron: "* * * * *", Timezone: "invalid/zone"}, {At: &now, Timezone: "UTC"}, {Cron: "TZ=UTC * * * *"}} {
		if _, err := s.next(now); !errors.Is(err, ErrInvalid) {
			t.Errorf("accepted %#v: %v", s, err)
		}
	}
	next, err = (Schedule{Cron: "30 2 * * *", Timezone: "America/New_York"}).next(time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC))
	if err != nil || !next.Equal(time.Date(2026, 3, 9, 6, 30, 0, 0, time.UTC)) {
		t.Fatalf("DST next=%s err=%v", next, err)
	}
	svc := NewService(testStore(t), &fakeExecutor{})
	d := definition()
	d.Schedule = Schedule{At: &now}
	if _, err := svc.validate(d, now); !errors.Is(err, ErrInvalid) {
		t.Fatal("past datetime accepted")
	}
	d = definition()
	d.Agent = "missing"
	if _, err := svc.Create(context.Background(), "tenant", d); !errors.Is(err, ErrInvalid) {
		t.Fatal("unknown agent accepted")
	}
}
func TestJournalRecoveryIsolationCASAndDelete(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "routines.jsonl")
	s, err := OpenJSONLStore(path)
	if err != nil {
		t.Fatal(err)
	}
	r := due(t, s)
	if other, err := OpenJSONLStore(path); err == nil {
		other.Close()
		t.Fatal("second writer acquired journal")
	}
	if _, err := s.Get(ctx, "other", r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx, r, 0); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	original := *r.Schedule.At
	*r.Schedule.At = time.Time{}
	stored, _ := s.Get(ctx, r.Namespace, r.ID)
	if !stored.Schedule.At.Equal(original) {
		t.Fatal("store aliases caller memory")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	_, _ = f.WriteString(`{"routine":`)
	_ = f.Close()
	s, err = OpenJSONLStore(path)
	if err != nil {
		t.Fatal(err)
	}
	stored, err = s.Get(ctx, r.Namespace, r.ID)
	if err != nil || !stored.Schedule.At.Equal(original) {
		t.Fatalf("replay: %+v %v", stored, err)
	}
	if err := s.Delete(ctx, r.Namespace, r.ID, stored.Version); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s, err = OpenJSONLStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, _ := s.List(ctx, "")
	if len(rows) != 0 {
		t.Fatal("deleted routine resurrected")
	}
}
func TestJournalRejectsCorruptCompleteLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routines.jsonl")
	_ = os.WriteFile(path, []byte("{broken}\n"), 0600)
	if _, err := OpenJSONLStore(path); err == nil {
		t.Fatal("corrupt journal accepted")
	}
}
func TestLocalExecutionNeverWritesDefinitionStore(t *testing.T) {
	store := testStore(t)
	r := due(t, store)
	before, err := os.ReadFile(store.file.Name())
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	ex := &fakeExecutor{execute: func(_ context.Context, got Routine, run Run) error {
		calls.Add(1)
		if got.Instruction != r.Instruction || run.ID == "" || run.AgentRunID == "" {
			return errors.New("bad dispatch")
		}
		return nil
	}}
	svc := NewService(store, ex)
	stateStore := testStateStore(t)
	scheduler := NewLocalScheduler(svc, stateStore, SchedulerConfig{PollInterval: time.Millisecond})
	stop := startScheduler(t, scheduler)
	await(t, func() bool { return completed(scheduler, r) })
	stop()
	got := state(t, scheduler, r)
	if got.LastRun.Status != "succeeded" || !got.NextRunAt.IsZero() || got.Pending != nil || calls.Load() != 1 {
		t.Fatalf("%+v calls=%d", got, calls.Load())
	}
	after, _ := os.ReadFile(store.file.Name())
	if string(before) != string(after) {
		t.Fatal("scheduler wrote to definition JSONL")
	}
	for _, forbidden := range []string{"next_run_at", "last_run", "pending", "attempts"} {
		if strings.Contains(string(after), forbidden) {
			t.Fatalf("JSONL contains %s", forbidden)
		}
	}
	persisted, _ := svc.Get(context.Background(), r.Namespace, r.ID)
	if !persisted.Enabled || persisted.Version != r.Version {
		t.Fatal("scheduler mutated definition")
	}
	// Both scheduler and state-store instances can be recreated without repeating completion.
	stop = startScheduler(t, scheduler)
	time.Sleep(20 * time.Millisecond)
	stop()
	if calls.Load() != 1 {
		t.Fatal("reran completed local state")
	}
	path := stateStore.file.Name()
	if err := stateStore.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenJSONLStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	fresh := NewLocalScheduler(svc, reopened, SchedulerConfig{PollInterval: time.Millisecond})
	stop = startScheduler(t, fresh)
	await(t, func() bool { return svc.running.Load() })
	time.Sleep(20 * time.Millisecond)
	stop()
	if !completed(fresh, r) || calls.Load() != 1 {
		t.Fatal("lost persistent completion")
	}

}
func TestLocalRetriesReuseOccurrence(t *testing.T) {
	store := testStore(t)
	r := due(t, store)
	ids := make(chan string, 3)
	ex := &fakeExecutor{execute: func(_ context.Context, _ Routine, run Run) error {
		ids <- run.ID
		return errors.New("upstream unavailable")
	}}
	scheduler := NewLocalScheduler(NewService(store, ex), testStateStore(t), SchedulerConfig{PollInterval: time.Millisecond, RetryDelay: time.Millisecond, MaxAttempts: 3})
	startScheduler(t, scheduler)
	await(t, func() bool { return completed(scheduler, r) })
	got := state(t, scheduler, r)
	if got.LastRun.Status != "failed" || got.LastRun.Attempts != 3 {
		t.Fatalf("%+v", got)
	}
	for i := 0; i < 3; i++ {
		if id := <-ids; id != got.LastRun.ID {
			t.Fatal("retry changed occurrence ID")
		}
	}
}
func TestLocalPauseCancelsAndDeleteRemovesState(t *testing.T) {
	store := testStore(t)
	r := due(t, store)
	started := make(chan struct{})
	cancelled := make(chan struct{})
	ex := &fakeExecutor{execute: func(ctx context.Context, _ Routine, _ Run) error {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	}}
	svc := NewService(store, ex)
	scheduler := NewLocalScheduler(svc, testStateStore(t), SchedulerConfig{PollInterval: time.Millisecond})
	startScheduler(t, scheduler)
	<-started
	if _, err := svc.SetEnabled(context.Background(), r.Namespace, r.ID, false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("pause did not cancel")
	}
	await(t, func() bool {
		v, err := scheduler.Status(context.Background(), r.Namespace, r.ID)
		return err == nil && v.DefinitionVersion > r.Version && v.Pending == nil && v.NextRunAt.IsZero()
	})
	if err := svc.Delete(context.Background(), r.Namespace, r.ID); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool {
		_, err := scheduler.Status(context.Background(), r.Namespace, r.ID)
		return errors.Is(err, ErrNotFound)
	})
}
func TestLocalRecoveryAndTimeout(t *testing.T) {
	for _, paused := range []bool{false, true} {
		t.Run(fmt.Sprint(paused), func(t *testing.T) {
			store := testStore(t)
			r := due(t, store)
			ex := &fakeExecutor{execute: func(ctx context.Context, _ Routine, _ Run) error {
				if paused {
					return ErrPaused
				}
				<-ctx.Done()
				return ctx.Err()
			}}
			scheduler := NewLocalScheduler(NewService(store, ex), testStateStore(t), SchedulerConfig{PollInterval: time.Millisecond, RunTimeout: time.Millisecond, MaxAttempts: 1})
			startScheduler(t, scheduler)
			await(t, func() bool { return completed(scheduler, r) })
			got := state(t, scheduler, r)
			if paused {
				if !got.Paused || got.LastRun.Status != "paused" {
					t.Fatalf("%+v", got)
				}
			} else if got.LastRun.Status != "failed" {
				t.Fatalf("%+v", got)
			}
		})
	}
}
func TestCronCatchUpAndExhaustedRecovery(t *testing.T) {
	store := testStore(t)
	r, err := NewService(store, &fakeExecutor{}).Create(context.Background(), "tenant", definition())
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	ex := &fakeExecutor{execute: func(context.Context, Routine, Run) error { calls.Add(1); return nil }}
	scheduler := NewLocalScheduler(NewService(store, ex), testStateStore(t), SchedulerConfig{PollInterval: time.Millisecond, MaxAttempts: 3})
	entry, _ := newScheduled(r, time.Now())
	entry.State.NextRunAt = time.Now().Add(-24 * time.Hour)
	entry.State.Pending = &Run{ID: "interrupted", Attempts: 3, Status: "running", RetryAt: time.Now().Add(-time.Hour)}
	release, err := scheduler.stateStore.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.engine.backend.Save(context.Background(), key(r.Namespace, r.ID), entry); err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	startScheduler(t, scheduler)
	await(t, func() bool { return completed(scheduler, r) })
	got := state(t, scheduler, r)
	if got.LastRun.Status != "failed" || got.LastRun.Attempts != 3 || calls.Load() != 0 || !got.NextRunAt.After(time.Now()) {
		t.Fatalf("%+v calls=%d", got, calls.Load())
	}
}
func TestServiceRejectsMultipleSchedulers(t *testing.T) {
	svc := NewService(testStore(t), &fakeExecutor{})
	local := NewLocalScheduler(svc, testStateStore(t), SchedulerConfig{})
	startScheduler(t, local)
	await(t, func() bool { return svc.running.Load() })
	if err := local.Run(context.Background()); err == nil {
		t.Fatal("duplicate Run accepted")
	}
	if err := NewLocalScheduler(svc, testStateStore(t), SchedulerConfig{}).Run(context.Background()); err == nil {
		t.Fatal("second backend accepted")
	}
}
func TestHTTPAndToolsShareServiceAndNamespace(t *testing.T) {
	svc := NewService(testStore(t), &fakeExecutor{})
	handler := NewHTTPHandler(svc, HTTPConfig{NamespaceResolver: func(r *http.Request) (string, error) {
		if r.Header.Get("X-Tenant") == "denied" {
			return "", errors.New("denied")
		}
		return r.Header.Get("X-Tenant"), nil
	}})
	request := func(method, path, tenant, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-Tenant", tenant)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}
	b, _ := json.Marshal(definition())
	w := request("POST", "/routines", "tenant", string(b))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var r Routine
	_ = json.Unmarshal(w.Body.Bytes(), &r)
	if w := request("GET", "/routines/"+r.ID, "other", ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
	if w := request("GET", "/routines", "denied", ""); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w := request("POST", "/routines", "tenant", string(b)+` {}`); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := request("POST", "/routines", "tenant", `{"namespace":"other"}`); w.Code != 400 {
		t.Fatal(w.Code)
	}
	factory := Tools(svc)
	for _, tool := range []agents.Tool{factory.PauseRoutineTool(), factory.CreateRoutineTool()} {
		name := tool.GetToolDescriptor().ToolUnion.OfFunction.Name
		if name == "routines_pause" {
			call := &agents.ToolCall{Namespace: "other", FunctionCallMessage: &responses.FunctionCallMessage{Arguments: fmt.Sprintf(`{"id":%q}`, r.ID)}}
			if _, err := tool.Execute(context.Background(), call); !errors.Is(err, ErrNotFound) {
				t.Fatal(err)
			}
			call.Namespace = "tenant"
			if _, err := tool.Execute(context.Background(), call); err != nil {
				t.Fatal(err)
			}
		}
		if name == "routines_create" {
			call := &agents.ToolCall{Namespace: "tenant", FunctionCallMessage: &responses.FunctionCallMessage{Arguments: fmt.Sprintf(`{"routine":%s}`, b)}}
			if _, err := tool.Execute(context.Background(), call); err != nil {
				t.Fatal(err)
			}
			call.Arguments = `{"routine":null}`
			if _, err := tool.Execute(context.Background(), call); err == nil {
				t.Fatal("null definition accepted")
			}
		}
	}
	got, _ := svc.Get(context.Background(), "tenant", r.ID)
	if got.Enabled {
		t.Fatal("tool failed to pause")
	}
	if w := request("POST", "/routines/"+r.ID+"/resume", "tenant", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("PUT", "/routines/"+r.ID, "tenant", string(b)); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("GET", "/routines/agents", "tenant", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "assistant") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("DELETE", "/routines/"+r.ID, "tenant", ""); w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestSchedulerStatusAPIAndToolIsolation(t *testing.T) {
	store := testStore(t)
	r := due(t, store)
	svc := NewService(store, &fakeExecutor{})
	scheduler := NewLocalScheduler(svc, testStateStore(t), SchedulerConfig{PollInterval: time.Millisecond})
	startScheduler(t, scheduler)
	await(t, func() bool { return completed(scheduler, r) })
	handler := NewHTTPHandler(svc, HTTPConfig{Scheduler: scheduler, NamespaceResolver: func(r *http.Request) (string, error) { return r.Header.Get("X-Tenant"), nil }})
	for _, ns := range []string{"tenant", "other"} {
		req := httptest.NewRequest("GET", "/routines/"+r.ID+"/status", nil)
		req.Header.Set("X-Tenant", ns)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if ns == "tenant" {
			if w.Code != 200 || !strings.Contains(w.Body.String(), "succeeded") {
				t.Fatal(w.Code, w.Body.String())
			}
		} else if w.Code != 404 {
			t.Fatal("cross-namespace status exposed")
		}
	}
	tool := Tools(svc, scheduler).GetRoutineStatusTool()
	call := &agents.ToolCall{Namespace: "tenant", FunctionCallMessage: &responses.FunctionCallMessage{Arguments: fmt.Sprintf(`{"id":%q}`, r.ID)}}
	result, err := tool.Execute(context.Background(), call)
	if err != nil || !strings.Contains(*result.Output.OfString, "succeeded") {
		t.Fatal(result, err)
	}
	call.Namespace = "other"
	if _, err = tool.Execute(context.Background(), call); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-namespace tool exposed status", err)
	}

}

type brokenState struct{ stateBackend }

func (b brokenState) Save(context.Context, string, scheduledRoutine) error {
	return errors.New("state unavailable")
}
func TestSchedulerStateFailureStopsBeforeExecution(t *testing.T) {
	store := testStore(t)
	due(t, store)
	var calls atomic.Int32
	ex := &fakeExecutor{execute: func(context.Context, Routine, Run) error { calls.Add(1); return nil }}
	scheduler := NewLocalScheduler(NewService(store, ex), testStateStore(t), SchedulerConfig{})
	scheduler.engine.backend = brokenState{scheduler.engine.backend}
	if err := scheduler.Run(context.Background()); err == nil || calls.Load() != 0 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}
