package routines_test

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/routines"
)

// This adapter deliberately lives outside package routines. Implementations can
// satisfy the public contract without referring to private scheduler types.
type recordingStateStore struct {
	routines.StateStore
	saves atomic.Int32
}

func (s *recordingStateStore) Save(ctx context.Context, rec routines.StateRecord) error {
	s.saves.Add(1)
	return s.StateStore.Save(ctx, rec)
}

type executor struct{}

func (executor) HasAgent(string) bool                                          { return true }
func (executor) Execute(context.Context, routines.Routine, routines.Run) error { return nil }

func TestExternalStateStorePlugin(t *testing.T) {
	defs, err := routines.OpenJSONLStore(filepath.Join(t.TempDir(), "routines.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer defs.Close()
	disk, err := routines.OpenJSONLStateStore(filepath.Join(t.TempDir(), "state.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	plugin := &recordingStateStore{StateStore: disk}
	var _ routines.StateStore = plugin
	svc := routines.NewService(defs, executor{})
	_, err = svc.Create(context.Background(), "tenant", routines.Definition{Name: "example", Agent: "agent", Instruction: "hello", Schedule: routines.Schedule{Cron: "0 9 * * *"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- routines.NewLocalScheduler(svc, plugin, routines.SchedulerConfig{PollInterval: time.Millisecond}).Run(ctx)
	}()
	deadline := time.Now().Add(time.Second)
	for plugin.saves.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if plugin.saves.Load() != 1 {
		t.Fatal("scheduler did not use injected state store")
	}
}
