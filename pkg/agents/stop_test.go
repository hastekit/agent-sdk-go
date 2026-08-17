package agents_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
)

// The generic exists for call sites whose result isn't a tool response —
// a Temporal activity interceptor only sees `any`. It must cancel and
// abandon the same way, and must not race the abandoned goroutine.
func TestRunStoppable_AbandonsAnyResult(t *testing.T) {
	const streamID = "generic-stream"

	broker := streambroker.NewMemoryStreamBroker()
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)

	go func() {
		<-started
		_ = broker.Stop(context.Background(), streamID)
	}()

	out, err := agents.RunStoppable(context.Background(), broker, 50*time.Millisecond, streamID,
		func(ctx context.Context) (any, error) {
			close(started)
			<-release // ignores ctx, like a tool that won't yield
			return "the real answer", nil
		})

	if !errors.Is(err, agents.ErrToolCancelled) {
		t.Fatalf("err = %v, want ErrToolCancelled", err)
	}
	if out != nil {
		t.Fatalf("abandoned call returned %v, want the zero value", out)
	}
}

// A call that finishes before the stop lands keeps its real result.
func TestRunStoppable_KeepsResultThatBeatTheStop(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()

	out, err := agents.RunStoppable(context.Background(), broker, 0, "quiet-stream",
		func(ctx context.Context) (any, error) { return "done", nil })

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "done" {
		t.Fatalf("got %v, want done", out)
	}
}
