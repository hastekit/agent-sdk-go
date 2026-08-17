package streambroker_test

import (
	"context"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
)

func TestMemoryBrokerWatchStop(t *testing.T) {
	ctx := context.Background()
	b := streambroker.NewMemoryStreamBroker()

	// Watch before stop: open, then closed by Stop.
	ch, release := b.WatchStop(ctx, "c1")
	defer release()
	select {
	case <-ch:
		t.Fatal("stop signal closed before Stop")
	default:
	}
	if err := b.Stop(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	<-ch
	// Idempotent (must not double-close).
	if err := b.Stop(ctx, "c1"); err != nil {
		t.Fatal(err)
	}

	// Watch after stop: already closed.
	late, releaseLate := b.WatchStop(ctx, "c1")
	defer releaseLate()
	<-late

	// A fresh run on the same channel gets an open signal again.
	started, err := b.EnqueueOrStart(ctx, "c1", nil)
	if err != nil || !started {
		t.Fatalf("EnqueueOrStart: started=%v err=%v", started, err)
	}
	fresh, releaseFresh := b.WatchStop(ctx, "c1")
	defer releaseFresh()
	select {
	case <-fresh:
		t.Fatal("fresh run inherited the previous run's stop")
	default:
	}
}
