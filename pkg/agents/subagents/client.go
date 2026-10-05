// Package subagents serves an agent's sub-agents from an agent registry.
package subagents

import (
	"context"
	"fmt"
	"slices"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
)

// Registry is where a Client finds agents by name. sdk.AgentRegistry is one.
type Registry interface {
	Agent(name string) (*agents.Agent, bool)
	AgentNames() []string
}

// Filter decides whether a caller may hand work to an agent, given who is
// asking and for which run. It sees each agent a Client would otherwise list,
// the caller's own entry (Self) included.
type Filter func(ctx context.Context, query agents.SubAgentQuery, agent agents.SubAgentInfo) bool

// Client is an agent's sub-agents, served from a Registry: the agents
// registered there, narrowed by its options, and optionally the caller itself.
// It mirrors skills.Client and mcpclient.Client, and is the
// agents.SubAgentClient to give an agent as AgentOptions.SubAgents.
//
// The registry is read on every listing, so agents registered after the client
// was made — the caller included — are found, and a client can be made before
// the agents it serves:
//
//	registry := sdk.NewRegistry()
//	researcher := sdk.MustNewAgent(&sdk.AgentConfig{Name: "researcher", Description: "Finds and summarizes sources.", ...})
//	lead := sdk.MustNewAgent(&sdk.AgentConfig{
//		Name:      "lead",
//		SubAgents: subagents.NewClient(registry, subagents.WithSelf()),
//		...
//	})
//	registry.Register(researcher)
//	registry.Register(lead)
//
// A caller is never listed as an agent of the registry; WithSelf lists it as
// itself, and a call to it runs on the calling agent.
type Client struct {
	registry Registry
	self     bool
	only     []string
	without  []string
	filters  []Filter
}

// Option configures a Client.
type Option func(*Client)

// NewClient serves every agent in registry but the caller, narrowed by opts.
func NewClient(registry Registry, opts ...Option) *Client {
	c := &Client{registry: registry}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c
}

// WithSelf lets the caller hand work to copies of itself, each working in a
// thread of its own, as it can to any other sub-agent.
func WithSelf() Option {
	return func(c *Client) { c.self = true }
}

// WithAgents serves only the named agents of the registry. Called more than
// once, the names accumulate.
func WithAgents(names ...string) Option {
	return func(c *Client) { c.only = append(c.only, names...) }
}

// WithoutAgents leaves the named agents of the registry out.
func WithoutAgents(names ...string) Option {
	return func(c *Client) { c.without = append(c.without, names...) }
}

// WithFilter serves only the agents filter accepts. Filters accumulate, and an
// agent must pass every one.
func WithFilter(filter Filter) Option {
	return func(c *Client) {
		if filter != nil {
			c.filters = append(c.filters, filter)
		}
	}
}

// ListSubAgents lists the caller first, when WithSelf allows it, then the
// registry's agents by name.
func (c *Client) ListSubAgents(ctx context.Context, query agents.SubAgentQuery) ([]agents.SubAgentInfo, error) {
	var listed []agents.SubAgentInfo
	if c.self && query.Caller != "" {
		self := agents.SubAgentInfo{Name: query.Caller, Self: true}
		if caller, ok := c.lookup(query.Caller); ok {
			self.Description = caller.Description()
		}
		if c.accepts(ctx, query, self) {
			listed = append(listed, self)
		}
	}
	if c.registry == nil {
		return listed, nil
	}

	names := slices.Clone(c.registry.AgentNames())
	slices.Sort(names)
	for _, name := range names {
		if name == query.Caller || slices.Contains(c.without, name) || (c.only != nil && !slices.Contains(c.only, name)) {
			continue
		}
		agent, ok := c.lookup(name)
		if !ok {
			continue
		}
		info := agents.SubAgentInfo{Name: name, Description: agent.Description()}
		if c.accepts(ctx, query, info) {
			listed = append(listed, info)
		}
	}
	return listed, nil
}

// RunSubAgent delivers req to the agent it names: the caller itself for its
// own entry, an agent of the registry otherwise.
func (c *Client) RunSubAgent(ctx context.Context, req agents.SubAgentRequest) (agents.AgentTaskOutcome, error) {
	agent, err := c.resolve(ctx, req)
	if err != nil {
		return agents.AgentTaskOutcome{}, err
	}
	return agents.RunAgentTask(ctx, agent, req.Input)
}

// SteerSubAgent adds req's message to the run going on the sub-agent's thread.
func (c *Client) SteerSubAgent(ctx context.Context, req agents.SubAgentRequest) (bool, error) {
	agent, err := c.resolve(ctx, req)
	if err != nil {
		return false, err
	}
	return agents.SteerLocalSubAgent(ctx, agent, req)
}

// resolve is the agent req names, if this client would list it for the
// caller's run.
func (c *Client) resolve(ctx context.Context, req agents.SubAgentRequest) (*agents.Agent, error) {
	if req.Caller == nil || req.Input == nil {
		return nil, fmt.Errorf("sub-agent request needs its caller and input")
	}
	listed, err := c.ListSubAgents(ctx, agents.SubAgentQuery{Caller: req.Caller.Name, Namespace: req.Input.Namespace, RunContext: req.Input.RunContext})
	if err != nil {
		return nil, err
	}
	for _, info := range listed {
		if info.Name != req.Name {
			continue
		}
		if info.Self {
			return req.Caller, nil
		}
		if agent, ok := c.lookup(info.Name); ok {
			return agent, nil
		}
	}
	return nil, fmt.Errorf("%s may not call sub-agent %q", req.Caller.Name, req.Name)
}

func (c *Client) lookup(name string) (*agents.Agent, bool) {
	if c.registry == nil {
		return nil, false
	}
	agent, ok := c.registry.Agent(name)
	return agent, ok && agent != nil
}

func (c *Client) accepts(ctx context.Context, query agents.SubAgentQuery, info agents.SubAgentInfo) bool {
	for _, filter := range c.filters {
		if !filter(ctx, query, info) {
			return false
		}
	}
	return true
}

var _ agents.SubAgentClient = (*Client)(nil)
