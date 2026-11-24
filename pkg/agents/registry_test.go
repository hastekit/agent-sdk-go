package agents_test

import (
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/stretchr/testify/require"
)

// codeAgents is a fixed set of agents built in code.
type codeAgents map[string]*agents.Agent

func (s codeAgents) Agent(name string) (*agents.Agent, bool) {
	a, ok := s[name]
	return a, ok
}

func (s codeAgents) AgentNames() []string {
	names := make([]string, 0, len(s))
	for name := range s {
		names = append(names, name)
	}
	return names
}

func TestStaticRegistryOffersTheSameAgentsToEveryNamespace(t *testing.T) {
	helper := agents.NewAgent(&agents.AgentOptions{Name: "helper"})
	writer := agents.NewAgent(&agents.AgentOptions{Name: "writer"})
	reg := agents.StaticRegistry(codeAgents{"writer": writer, "helper": helper})
	for _, namespace := range []string{"a", "b"} {
		agent, ok, err := reg.Agent(t.Context(), namespace, "helper")
		require.NoError(t, err)
		require.True(t, ok)
		require.Same(t, helper, agent)
		names, err := reg.AgentNames(t.Context(), namespace)
		require.NoError(t, err)
		require.Equal(t, []string{"helper", "writer"}, names, "sorted")
	}
	_, ok, err := reg.Agent(t.Context(), "a", "missing")
	require.NoError(t, err)
	require.False(t, ok)

	_, ok, err = agents.StaticRegistry(nil).Agent(t.Context(), "a", "helper")
	require.NoError(t, err)
	require.False(t, ok, "an empty set holds nothing")
}
