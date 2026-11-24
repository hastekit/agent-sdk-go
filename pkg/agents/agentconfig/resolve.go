package agentconfig

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
)

// Catalog reads configs from several stores with the precedence skills.Client
// uses: a built-in (any store's empty namespace) wins over a namespace's own
// config with the same name, and a name two stores both hold in one scope is
// an error rather than a silent choice.
type Catalog struct {
	stores []Store
}

// NewCatalog reads every store at lookup time, so configs written at run time
// are visible on the next lookup.
func NewCatalog(stores ...Store) *Catalog {
	return &Catalog{stores: append([]Store(nil), stores...)}
}

// Reads reports whether the catalog reads configs from store.
func (c *Catalog) Reads(store Store) bool {
	if store == nil {
		return false
	}
	// Comparing interfaces panics only when both hold the same uncomparable
	// type, so a store of such a type is never found.
	if !reflect.TypeOf(store).Comparable() {
		return false
	}
	for _, s := range c.stores {
		if s == store {
			return true
		}
	}
	return false
}

// getScope reads one scope's config from whichever store holds it.
func (c *Catalog) getScope(ctx context.Context, namespace, name string) (Config, bool, error) {
	var found *Config
	for _, store := range c.stores {
		if store == nil {
			return Config{}, false, errors.New("agent config store must not be nil")
		}
		cfg, err := store.Get(ctx, namespace, name)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return Config{}, false, err
		}
		if found != nil {
			return Config{}, false, duplicate(namespace, name)
		}
		if cfg.Name != name {
			return Config{}, false, fmt.Errorf("agent config store returned %q for %q", cfg.Name, name)
		}
		found = &cfg
	}
	if found == nil {
		return Config{}, false, nil
	}
	return *found, true, nil
}

func duplicate(namespace, name string) error {
	if namespace == "" {
		return fmt.Errorf("duplicate built-in agent %q", name)
	}
	return fmt.Errorf("duplicate agent %q in one namespace", name)
}

// Get resolves name for namespace: the built-in if there is one, else the
// namespace's own. builtIn reports which. An empty namespace resolves
// built-ins only.
func (c *Catalog) Get(ctx context.Context, namespace, name string) (cfg Config, builtIn bool, err error) {
	if err := checkNamespace(ctx, namespace); err != nil {
		return Config{}, false, err
	}
	cfg, ok, err := c.getScope(ctx, "", name)
	if err != nil || ok {
		return cfg, ok, err
	}
	if namespace == "" {
		return Config{}, false, ErrNotFound
	}
	cfg, ok, err = c.getScope(ctx, namespace, name)
	if err != nil {
		return Config{}, false, err
	}
	if !ok {
		return Config{}, false, ErrNotFound
	}
	return cfg, false, nil
}

// listScope lists one scope across every store, ordered by name.
func (c *Catalog) listScope(ctx context.Context, namespace string) ([]Config, error) {
	var listed []Config
	seen := map[string]bool{}
	for _, store := range c.stores {
		if store == nil {
			return nil, errors.New("agent config store must not be nil")
		}
		configs, err := listAll(ctx, store, namespace)
		if err != nil {
			return nil, err
		}
		for _, cfg := range configs {
			if seen[cfg.Name] {
				return nil, duplicate(namespace, cfg.Name)
			}
			seen[cfg.Name] = true
			listed = append(listed, cfg)
		}
	}
	sort.Slice(listed, func(i, j int) bool { return listed[i].Name < listed[j].Name })
	return listed, nil
}

// Listing is what one namespace can run, split by owner.
type Listing struct {
	// BuiltIn holds the built-in agents, shared by every namespace.
	BuiltIn []Config
	// Own holds the namespace's own agents that no built-in shadows.
	Own []Config
}

// List returns the built-ins and the namespace's own configs. A namespace's
// config with a built-in's name stays stored but never runs, so it is left out.
func (c *Catalog) List(ctx context.Context, namespace string) (Listing, error) {
	if err := checkNamespace(ctx, namespace); err != nil {
		return Listing{}, err
	}
	builtIns, err := c.listScope(ctx, "")
	if err != nil {
		return Listing{}, fmt.Errorf("list built-in agents: %w", err)
	}
	listing := Listing{BuiltIn: builtIns, Own: []Config{}}
	if namespace == "" {
		return listing, nil
	}
	shadowed := make(map[string]bool, len(builtIns))
	for _, cfg := range builtIns {
		shadowed[cfg.Name] = true
	}
	own, err := c.listScope(ctx, namespace)
	if err != nil {
		return Listing{}, fmt.Errorf("list agents: %w", err)
	}
	for _, cfg := range own {
		if !shadowed[cfg.Name] {
			listing.Own = append(listing.Own, cfg)
		}
	}
	return listing, nil
}

// Snapshot freezes the graph a run entering at name would use: the agent and
// every agent reachable through its handoffs and sub-agents, each resolved as
// Get does. A built-in's edges resolve among built-ins only, so what a built-in
// does never depends on which namespace runs it. A target that does not resolve
// is left out of the graph and its edge pruned from the snapshot's copy of the
// config; the agent still runs.
func (c *Catalog) Snapshot(ctx context.Context, namespace, name string) (*Snapshot, error) {
	root, rootBuiltIn, err := c.Get(ctx, namespace, name)
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
		resolved, targetBuiltIn, err := c.Get(ctx, scope, target)
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
