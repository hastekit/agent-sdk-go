package agentconfig

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
)

// Registry finds agents by namespace: agents built in code, which every
// namespace can run; then built-in configs, which every namespace can run and
// none can replace; then the namespace's own configs, read from the store.
// Each is built by the Builder. It is an agents.Registry:
//
//	registry, err := agentconfig.NewRegistry(builder, builtIns, userStore, codeAgents)
//	http.ListenAndServe(":8080", agui.NewHandler(registry, agui.WithAgentConfigs()))
//
// The store is read at lookup time, so configs written at run time are found
// on the next lookup.
//
// A code agent shadows a config with its name; keep the names apart, and
// ReservedNames stops a namespace from creating one. A config can neither hand
// off to a code agent nor call one as a sub-agent: a durable run carries its
// graph as configs.
type Registry struct {
	builder  *Builder
	builtIns map[string]Config
	store    MutableStore
	static   []agents.StaticAgents
}

var _ agents.Registry = (*Registry)(nil)

// NewRegistry looks agents up in static, in order, then among builtIns, then
// in store, and builds configs with builder, which is required. builtIns are
// validated and copied. store holds each namespace's own agents; nil offers
// built-ins only.
func NewRegistry(builder *Builder, builtIns []Config, store MutableStore, static ...agents.StaticAgents) (*Registry, error) {
	if builder == nil {
		return nil, errors.New("agentconfig: NewRegistry needs a Builder; wrap agents built in code alone with agents.StaticRegistry")
	}
	registry := &Registry{builder: builder, builtIns: make(map[string]Config, len(builtIns)), store: store, static: slices.Clone(static)}
	for _, c := range builtIns {
		if err := Validate(c); err != nil {
			return nil, err
		}
		if _, exists := registry.builtIns[c.Name]; exists {
			return nil, fmt.Errorf("%w: duplicate built-in agent %q", ErrInvalid, c.Name)
		}
		copy, err := clone(c)
		if err != nil {
			return nil, err
		}
		registry.builtIns[c.Name] = copy
	}
	return registry, nil
}

// Builder is the Builder the registry builds configs with.
func (r *Registry) Builder() *Builder { return r.builder }

// Store is where namespaces' own configs are read and written, or nil.
func (r *Registry) Store() MutableStore { return r.store }

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
	snapshot, err := r.Snapshot(ctx, namespace, name)
	if errors.Is(err, ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	agent, err := r.builder.Build(snapshot)
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
	listed, err := r.list(ctx, namespace)
	if err != nil {
		return nil, err
	}
	for _, cfg := range listed.builtIn {
		add(cfg.Name)
	}
	for _, cfg := range listed.own {
		add(cfg.Name)
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
	listed, err := r.list(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, cfg := range listed.builtIn {
		names = append(names, cfg.Name)
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
	globals, err := r.builder.GlobalSkillNames(ctx)
	if err != nil {
		return nil, err
	}
	names = append(names, globals...)
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
	return r.builder.Validate(cfg)
}

// Snapshot freezes the graph a run entering at name would use: the agent and
// every agent reachable through its handoffs and sub-agents, each resolved by
// the registry's precedence. A built-in's edges resolve among built-ins only,
// so what a built-in does never depends on which namespace runs it. A target
// that does not resolve is left out of the graph and its edge pruned from the
// snapshot's copy of the config; the agent still runs. It returns ErrNotFound
// for a name the namespace cannot run. Code agents are never in a snapshot.
func (r *Registry) Snapshot(ctx context.Context, namespace, name string) (*Snapshot, error) {
	root, rootBuiltIn, err := r.get(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	snapshot := &Snapshot{Namespace: namespace, Root: root.Name, Agents: map[string]Config{root.Name: root}}
	builtIn := map[string]bool{root.Name: rootBuiltIn}
	queue := []string{root.Name}
	// resolve reports whether cfg may keep its edge to target, adding the
	// target to the graph the first time it is reached.
	resolve := func(cfg Config, target, edge string) (bool, error) {
		if _, done := snapshot.Agents[target]; done {
			// Already resolved, and by name a namespace resolves it the same
			// way here. A built-in still may not reach a namespace's own.
			return !builtIn[cfg.Name] || builtIn[target], nil
		}
		scope := namespace
		if builtIn[cfg.Name] {
			scope = ""
		}
		resolved, targetBuiltIn, err := r.get(ctx, scope, target)
		if errors.Is(err, ErrNotFound) {
			slog.WarnContext(ctx, edge+" does not exist; the edge is left out of the run",
				slog.String("namespace", namespace), slog.String("agent", cfg.Name), slog.String("target", target))
			return false, nil
		}
		if err != nil {
			return false, err
		}
		snapshot.Agents[resolved.Name] = resolved
		builtIn[resolved.Name] = targetBuiltIn
		queue = append(queue, resolved.Name)
		return true, nil
	}
	for len(queue) > 0 {
		cfg := snapshot.Agents[queue[0]]
		queue = queue[1:]
		handoffs := make([]Handoff, 0, len(cfg.Handoffs))
		for _, h := range cfg.Handoffs {
			keep, err := resolve(cfg, h.Name, "handoff target")
			if err != nil {
				return nil, err
			}
			if keep {
				handoffs = append(handoffs, h)
			}
		}
		cfg.Handoffs = handoffs
		subAgents := make([]SubAgent, 0, len(cfg.SubAgents))
		for _, sub := range cfg.SubAgents {
			keep, err := resolve(cfg, sub.Name, "sub-agent")
			if err != nil {
				return nil, err
			}
			if keep {
				subAgents = append(subAgents, sub)
			}
		}
		cfg.SubAgents = subAgents
		snapshot.Agents[cfg.Name] = cfg
	}
	return snapshot, nil
}

// checkScope rejects a blank but nonempty namespace, so it cannot pass for the
// empty one, which resolves built-ins only.
func checkScope(ctx context.Context, namespace string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if namespace != "" && strings.TrimSpace(namespace) == "" {
		return fmt.Errorf("%w: blank namespace", ErrInvalid)
	}
	return nil
}

// get resolves name for namespace: the built-in if there is one, else the
// namespace's own. builtIn reports which. An empty namespace resolves
// built-ins only.
func (r *Registry) get(ctx context.Context, namespace, name string) (cfg Config, builtIn bool, err error) {
	if err := checkScope(ctx, namespace); err != nil {
		return Config{}, false, err
	}
	if cfg, ok := r.builtIns[name]; ok {
		copy, err := clone(cfg)
		return copy, true, err
	}
	if namespace == "" || r.store == nil {
		return Config{}, false, ErrNotFound
	}
	cfg, err = r.store.Get(ctx, namespace, name)
	if err != nil {
		return Config{}, false, err
	}
	if cfg.Name != name {
		return Config{}, false, fmt.Errorf("agent config store returned %q for %q", cfg.Name, name)
	}
	return cfg, false, nil
}

// listing is what one namespace can run, split by owner.
type listing struct {
	// builtIn holds the built-in agents, shared by every namespace.
	builtIn []Config
	// own holds the namespace's own agents that no built-in shadows.
	own []Config
}

// list returns the built-ins and the namespace's own configs, each ordered by
// name. A namespace's config with a built-in's name stays stored but never
// runs, so it is left out.
func (r *Registry) list(ctx context.Context, namespace string) (listing, error) {
	if err := checkScope(ctx, namespace); err != nil {
		return listing{}, err
	}
	builtIns, err := sortedClones(slices.Collect(maps.Values(r.builtIns)))
	if err != nil {
		return listing{}, err
	}
	listed := listing{builtIn: builtIns}
	if namespace == "" || r.store == nil {
		return listed, nil
	}
	own, err := r.store.List(ctx, namespace)
	if err != nil {
		return listing{}, fmt.Errorf("list agents: %w", err)
	}
	for _, cfg := range own {
		if _, shadowed := r.builtIns[cfg.Name]; !shadowed {
			listed.own = append(listed.own, cfg)
		}
	}
	return listed, nil
}
