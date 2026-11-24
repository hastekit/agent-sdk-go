package agui

import (
	"context"
	"net/http"
	"slices"
	"sort"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
)

// Registry finds the agents a namespace can run. The handler asks it on every
// request, with the namespace WithNamespaceResolver resolved, so agents may be
// created per namespace at run time: agentconfig.Registry builds them from
// stored configs, and another namespace never finds them. Wrap a fixed set of
// agents built in code with StaticRegistry.
//
// Implementations must be safe for concurrent requests.
type Registry interface {
	// Agent returns the agent namespace runs under name. ok is false when the
	// namespace has no such agent; an error means the lookup itself failed.
	Agent(ctx context.Context, namespace, name string) (agent *agents.Agent, ok bool, err error)
	// AgentNames lists the agents namespace can run.
	AgentNames(ctx context.Context, namespace string) ([]string, error)
}

// GlobalSkillLister is a Registry that can list every developer-owned global
// skill its agents use without building them. The /skills routes reserve
// these names, so a user's upload never sits shadowed by one. A Registry
// without it is asked for each of its agents' skills instead.
type GlobalSkillLister interface {
	GlobalSkillNames(ctx context.Context) ([]string, error)
}

// StaticAgents is a fixed set of agents built in code, such as
// *sdk.AgentRegistry.
type StaticAgents interface {
	Agent(name string) (*agents.Agent, bool)
	AgentNames() []string
}

// StaticRegistry offers the same agents to every namespace — the registry for
// agents built in code:
//
//	registry := hastekit.NewRegistry()
//	registry.Register(agent)
//	http.ListenAndServe(":8080", agui.NewHandler(agui.StaticRegistry(registry)))
func StaticRegistry(set StaticAgents) Registry {
	return staticRegistry{set: set}
}

type staticRegistry struct{ set StaticAgents }

func (s staticRegistry) Agent(_ context.Context, _ string, name string) (*agents.Agent, bool, error) {
	if s.set == nil {
		return nil, false, nil
	}
	agent, ok := s.set.Agent(name)
	return agent, ok, nil
}

func (s staticRegistry) AgentNames(context.Context, string) ([]string, error) {
	if s.set == nil {
		return nil, nil
	}
	names := slices.Clone(s.set.AgentNames())
	sort.Strings(names)
	return names, nil
}

// GlobalSkillNames asks each agent for its globals. They are already built,
// so this builds nothing.
func (s staticRegistry) GlobalSkillNames(ctx context.Context) ([]string, error) {
	if s.set == nil {
		return nil, nil
	}
	var names []string
	for _, name := range s.set.AgentNames() {
		agent, ok := s.set.Agent(name)
		if !ok {
			continue
		}
		skills, err := agentGlobalSkills(ctx, agent, nil)
		if err != nil {
			return nil, err
		}
		for _, skill := range skills {
			names = append(names, skill.Name)
		}
	}
	return names, nil
}

// lookupAgent resolves the {agent} path value in the request's namespace,
// answering the request itself when there is no agent to hand back.
func lookupAgent(w http.ResponseWriter, r *http.Request, registry Registry) (*agents.Agent, bool) {
	agent, ok, err := registry.Agent(r.Context(), requestNamespace(r), r.PathValue("agent"))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "unable to load agent")
		return nil, false
	}
	if !ok || agent == nil {
		writeJSONError(w, http.StatusNotFound, "agent not found")
		return nil, false
	}
	return agent, true
}
