package agents

import (
	"context"
	"slices"
	"sort"
)

// Registry finds the agents a namespace can run. Handlers and schedulers ask
// it on every request, with the namespace they authenticated, so agents may be
// created per namespace at run time: agentconfig.Registry builds them from
// stored configs, and another namespace never finds them. Wrap a fixed set of
// agents built in code with StaticRegistry.
//
// Implementations must be safe for concurrent callers.
type Registry interface {
	// Agent returns the agent namespace runs under name. ok is false when the
	// namespace has no such agent; an error means the lookup itself failed.
	Agent(ctx context.Context, namespace, name string) (agent *Agent, ok bool, err error)
	// AgentNames lists the agents namespace can run.
	AgentNames(ctx context.Context, namespace string) ([]string, error)
}

// StaticAgents is a fixed set of agents built in code, such as
// *hastekit.AgentRegistry.
type StaticAgents interface {
	Agent(name string) (*Agent, bool)
	AgentNames() []string
}

// StaticRegistry offers the same agents to every namespace — the Registry for
// agents built in code:
//
//	registry := hastekit.NewRegistry()
//	registry.Register(agent)
//	http.ListenAndServe(":8080", agui.NewHandler(agents.StaticRegistry(registry)))
func StaticRegistry(set StaticAgents) Registry {
	return staticRegistry{set: set}
}

type staticRegistry struct{ set StaticAgents }

func (s staticRegistry) Agent(_ context.Context, _ string, name string) (*Agent, bool, error) {
	if s.set == nil {
		return nil, false, nil
	}
	agent, ok := s.set.Agent(name)
	return agent, ok, nil
}

// AgentNames lists the set's agents, sorted.
func (s staticRegistry) AgentNames(context.Context, string) ([]string, error) {
	if s.set == nil {
		return nil, nil
	}
	names := slices.Clone(s.set.AgentNames())
	sort.Strings(names)
	return names, nil
}
