package sdk

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/workflow"
	"github.com/stretchr/testify/require"
)

func TestSharedRegistryWorkflows(t *testing.T) {
	registry := NewRegistry()
	require.NoError(t, registry.Register(&agents.Agent{Name: "same"}))
	compiled, err := workflow.LoadYAML([]byte("version: 1\nid: flow\nnodes: [{id: result, type: javascript, config: {code: 'return input.value;'}}]"), workflow.Dependencies{})
	require.NoError(t, err)
	require.NoError(t, registry.RegisterWorkflow("same", compiled))
	require.ErrorIs(t, registry.RegisterWorkflow("same", compiled), workflow.ErrAlreadyRegistered)
	require.Equal(t, []string{"same"}, registry.WorkflowNames())
	require.Equal(t, []string{"same"}, registry.AgentNames())
	got, ok := registry.Workflow("same")
	require.True(t, ok)
	require.Same(t, compiled, got)
	state, err := registry.RunWorkflow(context.Background(), "same", &workflow.Input{RunContext: map[string]any{"input": map[string]any{"value": 42}}})
	require.NoError(t, err)
	require.Equal(t, float64(42), state.RunContext["nodes"].(map[string]any)["result"])
	handler := NewWorkflowHTTPHandler(registry, workflow.HTTPConfig{})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/workflows", nil))
	require.Equal(t, 200, w.Code)
	require.JSONEq(t, `{"workflows":["same"]}`, w.Body.String())
}
