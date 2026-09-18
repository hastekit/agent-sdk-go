package agents_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

type executionRuntimeFunc func(context.Context, *agents.Agent, *agents.AgentInput) (*agents.AgentOutput, error)

func (f executionRuntimeFunc) Run(ctx context.Context, a *agents.Agent, in *agents.AgentInput) (*agents.AgentOutput, error) {
	return f(ctx, a, in)
}

func TestWaitWithoutReadingStreamDoesNotBlockExecution(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()
	result := &agents.AgentOutput{RunID: "finished"}
	a := agents.NewAgent(&agents.AgentOptions{Name: "test", StreamBroker: broker,
		Runtime: executionRuntimeFunc(func(ctx context.Context, a *agents.Agent, in *agents.AgentInput) (*agents.AgentOutput, error) {
			for i := 0; i < agents.StreamBufferSize*4; i++ {
				if err := broker.Publish(ctx, in.StreamID, textItemDone("id", "text")); err != nil {
					return nil, err
				}
			}
			// Deliberately no broker.Close: even runtime failure/return must release subscriptions.
			return result, nil
		})})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	h, err := a.Execute(ctx, &agents.AgentInput{})
	require.NoError(t, err)
	out, err := h.Wait(ctx)
	require.Same(t, result, out)
	require.ErrorIs(t, err, agents.ErrStreamOverflow)
	n := 0
	for range h.Chunks {
		n++
	}
	require.Equal(t, agents.StreamBufferSize, n)
	out, err = h.Wait(ctx)
	require.Same(t, result, out)
	require.ErrorIs(t, err, agents.ErrStreamOverflow)
	// Non-streaming callers neither subscribe nor overflow.
	out, err = a.Run(ctx, &agents.AgentInput{})
	require.NoError(t, err)
	require.Same(t, result, out)
}

func TestExecuteWaitAndStopHaveSeparateLifetimes(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()
	entered := make(chan context.Context, 1)
	a := agents.NewAgent(&agents.AgentOptions{Name: "test", StreamBroker: broker,
		Runtime: executionRuntimeFunc(func(ctx context.Context, a *agents.Agent, in *agents.AgentInput) (*agents.AgentOutput, error) {
			watch, cancel := agents.StopCancelContext(ctx, broker, in.StreamID)
			defer cancel()
			entered <- ctx
			<-watch.Done()
			return &agents.AgentOutput{RunID: "canceled"}, nil
		})})
	startCtx, cancelStart := context.WithCancel(context.Background())
	h, err := a.Execute(startCtx, &agents.AgentInput{})
	require.NoError(t, err)
	runCtx := <-entered
	cancelStart()
	require.NoError(t, runCtx.Err())
	waitCtx, cancelWait := context.WithCancel(context.Background())
	cancelWait()
	_, err = h.Wait(waitCtx)
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, runCtx.Err())
	require.NoError(t, h.Stop(context.Background()))
	timeout, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := h.Wait(timeout)
	require.NoError(t, err)
	require.Equal(t, "canceled", out.RunID)
}

func TestRunCancelsLocalAndRemoteWork(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()
	entered := make(chan struct{})
	stopped := make(chan struct{})
	a := agents.NewAgent(&agents.AgentOptions{Name: "test", StreamBroker: broker,
		Runtime: executionRuntimeFunc(func(ctx context.Context, a *agents.Agent, in *agents.AgentInput) (*agents.AgentOutput, error) {
			remote, cancelRemote := agents.StopCancelContext(context.Background(), broker, in.StreamID)
			defer cancelRemote()
			close(entered)
			<-ctx.Done()
			<-remote.Done()
			close(stopped)
			return nil, ctx.Err()
		})})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := a.Run(ctx, &agents.AgentInput{})
		done <- err
	}()
	<-entered
	cancel()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("remote cancellation not delivered")
	}
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestStreamPreservesEventsAndRuntimeErrors(t *testing.T) {
	failure := errors.New("runtime failed")
	broker := streambroker.NewMemoryStreamBroker()
	a := agents.NewAgent(&agents.AgentOptions{Name: "test", StreamBroker: broker,
		Runtime: executionRuntimeFunc(func(ctx context.Context, a *agents.Agent, in *agents.AgentInput) (*agents.AgentOutput, error) {
			for i := 0; i < 10; i++ {
				if err := broker.Publish(ctx, in.StreamID, textItemDone("id", "text")); err != nil {
					return nil, err
				}
			}
			return nil, failure
		})})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	h, err := a.Execute(ctx, &agents.AgentInput{})
	require.NoError(t, err)
	n := 0
	for range h.Chunks {
		n++
	}
	require.Equal(t, 10, n)
	_, err = h.Wait(ctx)
	require.ErrorIs(t, err, failure)
	require.NotErrorIs(t, err, agents.ErrStreamOverflow)
}

func TestResultTextHandlesEmptyAndMixedContent(t *testing.T) {
	var nilResult *agents.AgentOutput
	require.Empty(t, nilResult.Text())
	require.Empty(t, (&agents.AgentOutput{}).Text())
	result := &agents.AgentOutput{Output: []responses.InputMessageUnion{
		responses.UserMessage("exclude user"),
		{OfOutputMessage: &responses.OutputMessage{}},
		{OfOutputMessage: &responses.OutputMessage{Content: &responses.OutputContent{
			{OfOutputText: &responses.OutputTextContent{Text: "hello"}}, {},
			{OfOutputText: &responses.OutputTextContent{Text: " world"}},
		}}},
	}}
	require.Equal(t, "hello world", result.Text())
}

func (executionRuntimeFunc) StreamBroker() agents.StreamBroker { return nil }

func (executionRuntimeFunc) RegisterAgent(*agents.AgentOptions) error { return nil }
