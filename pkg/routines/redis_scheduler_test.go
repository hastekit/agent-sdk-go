package routines

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func redisTestClient(t *testing.T) (*redis.Client, string) {
	t.Helper()
	addr := os.Getenv("HASTEKIT_REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("set HASTEKIT_REDIS_TEST_ADDR for Redis integration tests")
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
	prefix := "{routines-test-" + uuid.NewString() + "}"
	t.Cleanup(func() {
		_ = client.Del(context.Background(), prefix+":owner", prefix+":state", prefix+":due").Err()
		_ = client.Close()
	})
	return client, prefix
}
func TestRedisPersistsSchedulerStateOnly(t *testing.T) {
	client, prefix := redisTestClient(t)
	store := testStore(t)
	r := due(t, store)
	before, _ := os.ReadFile(store.file.Name())
	var calls atomic.Int32
	ex := &fakeExecutor{execute: func(context.Context, Routine, Run) error { calls.Add(1); return nil }}
	svc := NewService(store, ex)
	config := RedisSchedulerConfig{SchedulerConfig: SchedulerConfig{PollInterval: 5 * time.Millisecond}, Prefix: prefix, LeaseDuration: time.Second}
	first := NewRedisScheduler(svc, client, config)
	stop := startScheduler(t, first)
	await(t, func() bool { return completed(first, r) })
	stop()
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
	fresh := NewRedisScheduler(svc, client, config)
	stop = startScheduler(t, fresh)
	await(t, func() bool { return svc.running.Load() })
	time.Sleep(30 * time.Millisecond)
	stop()
	if calls.Load() != 1 || state(t, fresh, r).LastRun.Status != "succeeded" {
		t.Fatal("lost durable completion")
	}
	after, _ := os.ReadFile(store.file.Name())
	if string(before) != string(after) {
		t.Fatal("execution changed definition JSONL")
	}
	if n := client.ZCard(context.Background(), prefix+":due").Val(); n != 0 {
		t.Fatal("completed job still due", n)
	}
}
func TestRedisInterruptedRunRecoversAndFencesOldOwner(t *testing.T) {
	client, prefix := redisTestClient(t)
	store := testStore(t)
	r := due(t, store)
	started := make(chan Run, 1)
	ex := &fakeExecutor{execute: func(ctx context.Context, _ Routine, run Run) error { started <- run; <-ctx.Done(); return ctx.Err() }}
	svc := NewService(store, ex)
	config := RedisSchedulerConfig{SchedulerConfig: SchedulerConfig{PollInterval: 5 * time.Millisecond}, Prefix: prefix, LeaseDuration: 300 * time.Millisecond}
	first := NewRedisScheduler(svc, client, config)
	stop := startScheduler(t, first)
	run := <-started
	// Another process/service cannot take ownership of the same Redis scheduler.
	other := NewRedisScheduler(NewService(store, &fakeExecutor{}), client, config)
	if err := other.Run(context.Background()); !errors.Is(err, ErrSchedulerOwned) {
		t.Fatal(err)
	}
	stop()
	pending := state(t, first, r).Pending
	if pending == nil || pending.ID != run.ID {
		t.Fatal("lost interrupted occurrence")
	}
	recovered := make(chan Run, 1)
	fresh := NewRedisScheduler(NewService(store, &fakeExecutor{execute: func(_ context.Context, _ Routine, run Run) error { recovered <- run; return nil }}), client, config)
	stop = startScheduler(t, fresh)
	await(t, func() bool { return completed(fresh, r) })
	second := <-recovered
	if second.ID != run.ID || second.Attempts != 2 {
		t.Fatalf("%+v", second)
	}
	row, _ := newScheduled(r, time.Now())
	if err := first.backend.Save(context.Background(), key(r.Namespace, r.ID), row); !errors.Is(err, ErrSchedulerLeaseLost) {
		t.Fatal("stale owner wrote state", err)
	}
	stop()
}
func TestRedisLeaseLossCancelsExecution(t *testing.T) {
	client, prefix := redisTestClient(t)
	store := testStore(t)
	due(t, store)
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	svc := NewService(store, &fakeExecutor{execute: func(ctx context.Context, _ Routine, _ Run) error {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	}})
	scheduler := NewRedisScheduler(svc, client, RedisSchedulerConfig{SchedulerConfig: SchedulerConfig{PollInterval: 5 * time.Millisecond}, Prefix: prefix, LeaseDuration: 150 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()
	<-entered
	if err := client.Set(context.Background(), prefix+":owner", "replacement", time.Second).Err(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("lease loss did not cancel")
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrSchedulerLeaseLost) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop")
	}
	if owner := client.Get(context.Background(), prefix+":owner").Val(); owner != "replacement" {
		t.Fatal("old owner removed replacement lease")
	}
}
