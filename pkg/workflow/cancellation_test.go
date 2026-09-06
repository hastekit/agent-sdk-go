package workflow

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type cancelTestNode struct {
	BaseNode
	fn func(context.Context, *Input) (map[string]any, string, error)
}

func (n *cancelTestNode) Validate() error { return nil }
func (n *cancelTestNode) Execute(ctx context.Context, in *Input) (map[string]any, string, error) {
	return n.fn(ctx, in)
}
func cancellationNode(fn func(context.Context, *Input) (map[string]any, string, error)) Node {
	return &cancelTestNode{BaseNode: BaseNode{NodeType: "test"}, fn: fn}
}

type cancellationOutcome struct {
	out *Input
	err error
}

func awaitCancellation(t *testing.T, ch <-chan cancellationOutcome) cancellationOutcome {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("workflow did not return after cancellation")
		return cancellationOutcome{}
	}
}

func TestCancelledWorkflowDoesNotStartNodes(t *testing.T) {
	g := NewGraph("cancel-before-start")
	g.AddNode("node", cancellationNode(func(context.Context, *Input) (map[string]any, string, error) {
		t.Error("cancelled workflow started node")
		return nil, DefaultPort, nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := g.Invoke(ctx, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.NotNil(t, out)
	require.Equal(t, NodeStatusSkipped, out.Status["node"])
}

func TestCancellationUnwindsParallelWaveAndPreservesCompletedWork(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	cause := errors.New("user stopped workflow")
	started := make(chan struct{}, 2)
	g := NewGraph("cancel-wave")
	g.AddNode("before", cancellationNode(func(context.Context, *Input) (map[string]any, string, error) {
		return map[string]any{"saved": true}, DefaultPort, nil
	}))
	for _, id := range []string{"a", "b"} {
		g.AddNode(id, cancellationNode(func(ctx context.Context, _ *Input) (map[string]any, string, error) {
			started <- struct{}{}
			<-ctx.Done()
			// Even a node that mistakenly returns success must not publish late output.
			return map[string]any{"late": true}, DefaultPort, nil
		}))
		g.AddEdge("before", id)
		g.AddEdge(id, "after")
	}
	g.AddNode("after", cancellationNode(func(context.Context, *Input) (map[string]any, string, error) {
		t.Error("downstream node started")
		return nil, DefaultPort, nil
	}))
	result := make(chan cancellationOutcome, 1)
	go func() { out, err := g.Invoke(ctx, nil); result <- cancellationOutcome{out, err} }()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("parallel node did not start")
		}
	}
	cancel(cause)
	got := awaitCancellation(t, result)
	require.ErrorIs(t, got.err, context.Canceled)
	require.ErrorIs(t, got.err, cause)
	require.Equal(t, NodeStatusCompleted, got.out.Status["before"])
	require.Equal(t, true, got.out.RunContext["saved"])
	require.NotContains(t, got.out.RunContext, "late")
	for _, id := range []string{"a", "b"} {
		require.Equal(t, NodeStatusCancelled, got.out.Status[id])
	}
	require.Equal(t, NodeStatusSkipped, got.out.Status["after"])
	require.Nil(t, got.out.Pause)
}

func TestWorkflowDeadline(t *testing.T) {
	g := NewGraph("deadline")
	g.AddNode("wait", cancellationNode(func(ctx context.Context, _ *Input) (map[string]any, string, error) {
		<-ctx.Done()
		return nil, "", ctx.Err()
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	out, err := g.Invoke(ctx, nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, NodeStatusCancelled, out.Status["wait"])
}

func TestCancellationOverridesPause(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := NewGraph("cancel-pause")
	g.AddNode("gate", cancellationNode(func(context.Context, *Input) (map[string]any, string, error) {
		cancel()
		return nil, "", Pause(map[string]any{"question": "approve?"})
	}))
	out, err := g.Invoke(ctx, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, out.Pause)
	require.Equal(t, NodeStatusCancelled, out.Status["gate"])
}

func TestFailureCancelsPeerWithoutLosingOriginalError(t *testing.T) {
	failure := errors.New("node failed")
	started := make(chan struct{})
	g := NewGraph("failure-cancels-peers")
	g.AddNode("fail", cancellationNode(func(context.Context, *Input) (map[string]any, string, error) { <-started; return nil, "", failure }))
	g.AddNode("peer", cancellationNode(func(ctx context.Context, _ *Input) (map[string]any, string, error) {
		close(started)
		<-ctx.Done()
		return nil, "", ctx.Err()
	}))
	out, err := g.Invoke(context.Background(), nil)
	require.ErrorIs(t, err, failure)
	require.Equal(t, NodeStatusFailed, out.Status["fail"])
	require.Equal(t, NodeStatusCancelled, out.Status["peer"])
}

func TestCancellationWaitsForNodeCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	cleaning := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	g := NewGraph("cleanup")
	g.AddNode("node", cancellationNode(func(ctx context.Context, _ *Input) (map[string]any, string, error) {
		close(started)
		<-ctx.Done()
		close(cleaning)
		<-release
		return nil, "", ctx.Err()
	}))
	result := make(chan cancellationOutcome, 1)
	go func() { out, err := g.Invoke(ctx, nil); result <- cancellationOutcome{out, err} }()
	<-started
	cancel()
	<-cleaning
	select {
	case <-result:
		t.Fatal("workflow returned while node still used shared input")
	default:
	}
	close(release)
	require.ErrorIs(t, awaitCancellation(t, result).err, context.Canceled)
}

func TestCancelledRunCanRetryWithoutRepeatingCompletedNodes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	g := NewGraph("retry")
	g.AddNode("before", cancellationNode(func(context.Context, *Input) (map[string]any, string, error) {
		calls.Add(1)
		return nil, DefaultPort, nil
	}))
	g.AddNode("stop", cancellationNode(func(ctx context.Context, _ *Input) (map[string]any, string, error) {
		cancel()
		return nil, DefaultPort, ctx.Err()
	}))
	g.AddEdge("before", "stop")
	out, err := g.Invoke(ctx, nil)
	require.ErrorIs(t, err, context.Canceled)
	out, err = g.Invoke(context.Background(), out)
	require.NoError(t, err)
	require.Equal(t, int32(1), calls.Load())
	require.Equal(t, NodeStatusCompleted, out.Status["stop"])
}

func TestCancellationDuringRoutingPreventsNextWave(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := NewGraph("cancel-routing")
	g.AddNode("before", cancellationNode(func(context.Context, *Input) (map[string]any, string, error) {
		return map[string]any{"saved": true}, DefaultPort, nil
	}))
	g.AddNode("after", cancellationNode(func(context.Context, *Input) (map[string]any, string, error) {
		t.Error("node dispatched after router cancelled workflow")
		return nil, DefaultPort, nil
	}))
	g.AddConditionalEdge("before", func(*Input) string { cancel(); return "next" }, map[string]string{"next": "after"})
	out, err := g.Invoke(ctx, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, NodeStatusCompleted, out.Status["before"])
	require.Equal(t, NodeStatusSkipped, out.Status["after"])
	require.Equal(t, true, out.RunContext["saved"])
}
