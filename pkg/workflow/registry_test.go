package workflow

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func registryGraph(t *testing.T) *Compiled {
	t.Helper()
	compiled, err := LoadYAML([]byte(`version: 1
id: review
nodes:
 - {id: review, type: human, config: {message: Review}}
 - {id: finish, type: javascript, config: {code: 'return input.amount;'}}
edges: [{from: review, port: approved, to: finish}]
`), Dependencies{})
	require.NoError(t, err)
	return compiled
}
func TestRegistryIndependentExecutionAndResume(t *testing.T) {
	compiled := registryGraph(t)
	left, right := NewRegistry(), NewRegistry()
	require.NoError(t, left.Register("review", compiled))
	require.ErrorIs(t, left.Register("review", compiled), ErrAlreadyRegistered)
	require.NoError(t, right.Register("review", compiled))
	require.Equal(t, []string{"review"}, left.WorkflowNames())
	state, err := left.Execute(context.Background(), "review", &Input{RunContext: map[string]any{"input": map[string]any{"amount": 50}}})
	require.NoError(t, err)
	require.NotNil(t, state.Pause)
	state.SetResume("review", map[string]any{"action": "approve"})
	state, err = left.Execute(context.Background(), "review", state)
	require.NoError(t, err)
	require.Nil(t, state.Pause)
	require.Equal(t, float64(50), state.RunContext["nodes"].(map[string]any)["finish"])
	_, err = left.Execute(context.Background(), "missing", nil)
	require.ErrorIs(t, err, ErrWorkflowNotFound)
	var empty Registry
	require.NoError(t, empty.Register("zero", compiled))
	require.Error(t, empty.Register("", compiled))
}
func TestRegistryConcurrentRegistration(t *testing.T) {
	registry := NewRegistry()
	compiled := registryGraph(t)
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- registry.Register("same", compiled); _ = registry.WorkflowNames() }()
	}
	wg.Wait()
	close(errs)
	successes := 0
	for err := range errs {
		if err == nil {
			successes++
		} else {
			require.True(t, errors.Is(err, ErrAlreadyRegistered), fmt.Sprint(err))
		}
	}
	require.Equal(t, 1, successes)
}
func TestRegistryRetainsInvocationOptions(t *testing.T) {
	registry := NewRegistry()
	compiled := registryGraph(t)
	require.NoError(t, registry.Register("review", compiled, WithMaxSteps(1)))
	in := &Input{RunContext: map[string]any{"input": map[string]any{"amount": 1}}}
	in.SetResume("review", map[string]any{"action": "approve"})
	_, err := registry.Execute(context.Background(), "review", in)
	require.Error(t, err)
}
