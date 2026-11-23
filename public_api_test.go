package sdk

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/stretchr/testify/require"
)

func testFileHistory(t *testing.T) *History {
	t.Helper()
	h, err := OpenFileHistory(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	return h
}

func TestPublicConstructorsAndRegistry(t *testing.T) {
	_, err := NewAgent(nil)
	require.Error(t, err)
	_, err = NewAgent(&AgentConfig{Name: "missing-model"})
	require.Error(t, err)
	path := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0600))
	_, err = OpenFileHistory(filepath.Join(path, "child"))
	require.Error(t, err)
	model := NewLLMClient(testConfigs()).Model("OpenAI/test")
	a, err := NewAgent(&AgentConfig{Name: "same", LLM: model, Instruction: NewPrompt("Hello")})
	require.NoError(t, err)
	left, right := NewRegistry(), NewRegistry()
	require.Empty(t, left.AgentNames())
	require.NoError(t, left.Register(a))
	require.ErrorIs(t, left.Register(a), ErrAgentAlreadyRegistered)
	require.Empty(t, right.AgentNames())
	require.NoError(t, right.Register(a))
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			require.NoError(t, left.Register(&agents.Agent{Name: fmt.Sprint(i)}))
			left.Agent("same")
			left.AgentNames()
		}(i)
	}
	wg.Wait()
	require.Len(t, left.AgentNames(), 41)
}

func TestRuntimeRegistrationAndLifecycle(t *testing.T) {
	model := NewLLMClient(testConfigs()).Model("OpenAI/test")
	left, err := NewRestateRuntime("http://localhost:8080", streambroker.NewMemoryStreamBroker())
	require.NoError(t, err)
	right, err := NewRestateRuntime("http://localhost:8080", streambroker.NewMemoryStreamBroker())
	require.NoError(t, err)
	cfg := &AgentConfig{Name: "same", LLM: model}
	_, err = NewAgent(cfg, WithRuntime(left))
	require.NoError(t, err)
	_, err = NewAgent(cfg, WithRuntime(left))
	require.ErrorIs(t, err, ErrAgentAlreadyRegistered)
	_, err = NewAgent(cfg, WithRuntime(right))
	require.NoError(t, err)
	ctx, _, finish, err := left.lifecycle.begin(context.Background())
	require.NoError(t, err)
	_, err = NewAgent(&AgentConfig{Name: "late", LLM: model}, WithRuntime(left))
	require.Error(t, err)
	closed := make(chan struct{})
	go func() { left.Close(); close(closed) }()
	<-ctx.Done()
	finish()
	<-closed
	require.NoError(t, left.Close())
	_, err = NewAgent(cfg, WithRuntime(left))
	require.Error(t, err)
	require.NoError(t, right.Close())
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	require.True(t, errors.Is(left.Serve(canceled, "localhost:0"), context.Canceled))
}

func TestRestateServeReportsStartupFailureAndShutsDown(t *testing.T) {
	model := NewLLMClient(testConfigs()).Model("OpenAI/test")
	rt, err := NewRestateRuntime("http://localhost:8080", streambroker.NewMemoryStreamBroker())
	require.NoError(t, err)
	defer rt.Close()
	_, err = NewAgent(&AgentConfig{Name: "test", LLM: model}, WithRuntime(rt))
	require.NoError(t, err)
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := occupied.Addr().String()
	require.Error(t, rt.Serve(context.Background(), address))
	// Failed startup releases the serving state, allowing a retry.
	require.NoError(t, occupied.Close())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rt.Serve(ctx, address) }()
	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}, 3*time.Second, 10*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not finish shutdown")
	}
	conn, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
	if conn != nil {
		conn.Close()
	}
	require.Error(t, err)
}

func TestTemporalServeReturnsMissingClientError(t *testing.T) {
	rt := &TemporalRuntime{broker: streambroker.NewMemoryStreamBroker()}
	model := NewLLMClient(testConfigs()).Model("OpenAI/test")
	_, err := NewAgent(&AgentConfig{Name: "test", LLM: model}, WithRuntime(rt))
	require.NoError(t, err)
	require.ErrorContains(t, rt.Serve(context.Background()), "no temporal client")
	require.NoError(t, rt.Close())
}

func TestDurableRuntimesRequireExplicitBroker(t *testing.T) {
	// Temporal validation must happen before attempting a network connection.
	_, err := NewTemporalRuntime("localhost:1", nil)
	require.ErrorContains(t, err, "explicit stream broker")
	_, err = NewRestateRuntime("http://localhost:8080", nil)
	require.ErrorContains(t, err, "explicit stream broker")
	model := NewLLMClient(testConfigs()).Model("OpenAI/test")
	_, err = NewAgent(&AgentConfig{Name: "test", LLM: model}, WithRuntime(plainRuntime{}))
	require.ErrorContains(t, err, "explicit stream broker")
	// Local agents retain their convenient in-process default.
	a, err := NewAgent(&AgentConfig{Name: "local", LLM: model})
	require.NoError(t, err)
	require.NotNil(t, a.StreamBroker())
	broker := streambroker.NewMemoryStreamBroker()
	rt, err := NewRestateRuntime("http://localhost:8080", broker)
	require.NoError(t, err)
	defer rt.Close()
	a, err = NewAgent(&AgentConfig{Name: "explicit", LLM: model}, WithRuntime(rt))
	require.NoError(t, err)
	require.Same(t, broker, a.StreamBroker())
}
