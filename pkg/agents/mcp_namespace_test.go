package agents_test

import (
	"context"
	"testing"

	"github.com/hastekit/agent-sdk-go/internal/testutil"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

// namespaceToolset captures only the non-secret identity crossing MCP listing boundaries.
type namespaceToolset struct {
	context   map[string]any
	namespace string
}

func (*namespaceToolset) GetName() string { return "gmail" }

func (s *namespaceToolset) ListTools(_ context.Context, namespace string, runContext map[string]any) ([]agents.Tool, error) {
	s.context = runContext
	s.namespace = namespace
	return nil, nil
}

// AgentInput.Namespace is authoritative even when incoming run context contains another subject.
func TestMCPListingUsesExecutionNamespace(t *testing.T) {
	server := &namespaceToolset{}
	model := &scriptedLLM{script: []*responses.Response{textResponse("done")}}
	agent := agents.NewAgent(&agents.AgentOptions{Name: "agent", MCPClient: testutil.NewMCPClient([]agents.MCPToolset{server}...)}).WithLLM(model)
	runContext := map[string]any{"namespace": "spoofed", "other": "value"}
	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "authenticated-user", ThreadID: "thread", RunContext: runContext, Message: userMessage("hello"),
	})

	// Identity has its own parameter; the application's run context stays untouched.
	requireStatus(t, out, agentstate.RunStatusCompleted)
	require.Equal(t, "authenticated-user", server.namespace)
	require.Equal(t, "spoofed", server.context["namespace"])
	require.Equal(t, "value", server.context["other"])
	require.Equal(t, "spoofed", runContext["namespace"])
}
