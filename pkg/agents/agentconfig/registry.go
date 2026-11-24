package agentconfig

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
)

// StaticAgents is a set of agents built in code, such as *sdk.AgentRegistry.
type StaticAgents interface {
	Agent(name string) (*agents.Agent, bool)
	AgentNames() []string
}

// Registry finds agents by namespace: agents built in code, which every
// namespace can run, then configs through the Builder — built-ins and the
// namespace's own. It satisfies agui.Registry:
//
//	registry := agentconfig.NewRegistry(builder, codeAgents)
//	http.ListenAndServe(":8080", agui.NewHandler(registry, agui.WithAgentConfigStore(userStore)))
//
// A code agent shadows a config with its name; keep the names apart, and
// ReservedNames stops a namespace from creating one. A config can neither hand
// off to a code agent nor call one as a sub-agent: a durable run carries its
// graph as configs.
type Registry struct {
	builder *Builder
	static  []StaticAgents
}

// NewRegistry looks agents up in static, in order, and then builds configs
// with builder.
func NewRegistry(builder *Builder, static ...StaticAgents) *Registry {
	return &Registry{builder: builder, static: slices.Clone(static)}
}

// Builder is the Builder the registry builds configs with.
func (r *Registry) Builder() *Builder { return r.builder }

// Reads reports whether the registry finds configs in store, so that agents
// written there are agents it runs.
func (r *Registry) Reads(store Store) bool {
	return r.builder != nil && r.builder.Catalog().Reads(store)
}

func (r *Registry) staticAgent(name string) (*agents.Agent, bool) {
	for _, set := range r.static {
		if set == nil {
			continue
		}
		if agent, ok := set.Agent(name); ok {
			return agent, true
		}
	}
	return nil, false
}

// Agent returns the agent namespace runs under name. ok is false when the
// namespace has no such agent: another namespace's config is never found.
func (r *Registry) Agent(ctx context.Context, namespace, name string) (*agents.Agent, bool, error) {
	if agent, ok := r.staticAgent(name); ok {
		return agent, true, nil
	}
	if r.builder == nil {
		return nil, false, nil
	}
	agent, err := r.builder.Agent(ctx, namespace, name)
	if errors.Is(err, ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return agent, true, nil
}

// AgentNames lists every agent namespace can run, sorted.
func (r *Registry) AgentNames(ctx context.Context, namespace string) ([]string, error) {
	seen := map[string]bool{}
	var names []string
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	for _, set := range r.static {
		if set != nil {
			for _, name := range set.AgentNames() {
				add(name)
			}
		}
	}
	if r.builder != nil {
		listing, err := r.builder.Catalog().List(ctx, namespace)
		if err != nil {
			return nil, err
		}
		for _, cfg := range listing.BuiltIn {
			add(cfg.Name)
		}
		for _, cfg := range listing.Own {
			add(cfg.Name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// ReservedNames lists the names no namespace may give its own config: every
// code agent's and every built-in config's, since either would shadow it.
func (r *Registry) ReservedNames(ctx context.Context) ([]string, error) {
	var names []string
	for _, set := range r.static {
		if set != nil {
			names = append(names, set.AgentNames()...)
		}
	}
	if r.builder != nil {
		listing, err := r.builder.Catalog().List(ctx, "")
		if err != nil {
			return nil, err
		}
		for _, cfg := range listing.BuiltIn {
			names = append(names, cfg.Name)
		}
	}
	sort.Strings(names)
	return slices.Compact(names), nil
}

// GlobalSkillNames lists the global skills users' uploads must not reuse: the
// code agents' own, which they already hold, and every global in the
// Builder's skill stores. No config is built to find them.
func (r *Registry) GlobalSkillNames(ctx context.Context) ([]string, error) {
	var names []string
	for _, set := range r.static {
		if set == nil {
			continue
		}
		for _, name := range set.AgentNames() {
			agent, ok := set.Agent(name)
			if !ok {
				continue
			}
			catalog, err := agent.ListSkills(ctx, "", nil, agents.SkillSelection{})
			if err != nil {
				return nil, err
			}
			for _, skill := range catalog {
				if skill.Global {
					names = append(names, skill.Name)
				}
			}
		}
	}
	if r.builder != nil {
		globals, err := r.builder.GlobalSkillNames(ctx)
		if err != nil {
			return nil, err
		}
		names = append(names, globals...)
	}
	sort.Strings(names)
	return slices.Compact(names), nil
}

// Validate checks a namespace's config before it is stored: runnable with the
// Builder's dependencies, not shadowed by a code agent or built-in, and not
// handing off to a code agent.
func (r *Registry) Validate(ctx context.Context, cfg Config) error {
	reserved, err := r.ReservedNames(ctx)
	if err != nil {
		return err
	}
	if slices.Contains(reserved, cfg.Name) {
		return ErrReserved
	}
	for _, h := range cfg.Handoffs {
		if _, ok := r.staticAgent(h.Name); ok {
			return fmt.Errorf("%w: %q is built in code, and a config can hand off only to configs", ErrInvalid, h.Name)
		}
	}
	for _, sub := range cfg.SubAgents {
		if _, ok := r.staticAgent(sub.Name); ok {
			return fmt.Errorf("%w: %q is built in code, and a config's sub-agents can only be configs", ErrInvalid, sub.Name)
		}
	}
	if r.builder == nil {
		return Validate(cfg)
	}
	return r.builder.Validate(cfg)
}
