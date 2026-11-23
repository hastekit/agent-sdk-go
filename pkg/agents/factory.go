package agents

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"golang.org/x/sync/singleflight"
)

// AgentRef names one immutable version of a factory-built agent.
type AgentRef struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

func (r AgentRef) String() string {
	if r.Version == "" {
		return r.Name
	}
	return r.Name + "@" + r.Version
}

// AgentFactory builds agents from stored configuration. A ref must always build
// the same agent: durable runs pin the ref they started with and rebuild it after restarts.
type AgentFactory interface {
	Build(ctx context.Context, ref AgentRef) (*AgentOptions, error)
}

// AgentFactoryFunc adapts a function to AgentFactory.
type AgentFactoryFunc func(ctx context.Context, ref AgentRef) (*AgentOptions, error)

func (f AgentFactoryFunc) Build(ctx context.Context, ref AgentRef) (*AgentOptions, error) {
	return f(ctx, ref)
}

// ErrAgentNotFound is what a factory wraps when a ref does not exist; runtimes do not retry it.
var ErrAgentNotFound = errors.New("agent not found")

// DefaultAgentCacheSize bounds how many built agents a CachedAgentFactory keeps.
const DefaultAgentCacheSize = 256

// CachedAgentFactory keeps the most recently built refs. Refs are immutable, so entries never go stale.
type CachedAgentFactory struct {
	factory AgentFactory
	size    int
	group   singleflight.Group

	mu      sync.Mutex
	order   *list.List
	entries map[AgentRef]*list.Element
}

type cachedAgent struct {
	ref     AgentRef
	options *AgentOptions
}

// NewCachedAgentFactory caches factory's results; size <= 0 uses DefaultAgentCacheSize.
func NewCachedAgentFactory(factory AgentFactory, size int) *CachedAgentFactory {
	if cached, ok := factory.(*CachedAgentFactory); ok {
		return cached
	}
	if size <= 0 {
		size = DefaultAgentCacheSize
	}
	return &CachedAgentFactory{factory: factory, size: size, order: list.New(), entries: map[AgentRef]*list.Element{}}
}

// Build returns the cached options for ref, building them once. Callers must not mutate the result.
func (c *CachedAgentFactory) Build(ctx context.Context, ref AgentRef) (*AgentOptions, error) {
	if options, ok := c.lookup(ref); ok {
		return options, nil
	}
	value, err, _ := c.group.Do(ref.Name+"\x00"+ref.Version, func() (any, error) {
		if options, ok := c.lookup(ref); ok {
			return options, nil
		}
		options, err := c.factory.Build(ctx, ref)
		if err != nil {
			return nil, err
		}
		if options == nil {
			return nil, fmt.Errorf("%w: factory built nothing for %s", ErrAgentNotFound, ref)
		}
		c.store(ref, options)
		return options, nil
	})
	if err != nil {
		return nil, err
	}
	return value.(*AgentOptions), nil
}

func (c *CachedAgentFactory) lookup(ref AgentRef) (*AgentOptions, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[ref]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(element)
	return element.Value.(*cachedAgent).options, true
}

func (c *CachedAgentFactory) store(ref AgentRef, options *AgentOptions) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.entries[ref]; ok {
		element.Value.(*cachedAgent).options = options
		c.order.MoveToFront(element)
		return
	}
	c.entries[ref] = c.order.PushFront(&cachedAgent{ref: ref, options: options})
	for c.order.Len() > c.size {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.entries, oldest.Value.(*cachedAgent).ref)
	}
}

// BuildFactoryOptions builds ref and checks the result names the same agent.
func BuildFactoryOptions(ctx context.Context, factory AgentFactory, ref AgentRef) (*AgentOptions, error) {
	if factory == nil {
		return nil, errors.New("no agent factory registered")
	}
	if strings.TrimSpace(ref.Name) == "" {
		return nil, errors.New("agent ref has no name")
	}
	options, err := factory.Build(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("build agent %s: %w", ref, err)
	}
	if options == nil {
		return nil, fmt.Errorf("build agent %s: %w", ref, ErrAgentNotFound)
	}
	if options.Name != "" && options.Name != ref.Name {
		return nil, fmt.Errorf("build agent %s: factory returned agent %q", ref, options.Name)
	}
	if options.LLM == nil {
		return nil, fmt.Errorf("build agent %s: agent model is required", ref)
	}
	return options, nil
}

// HandoffRef is the factory ref a handoff targets.
func HandoffRef(h *Handoff) AgentRef {
	if target, ok := h.Agent.(*Agent); ok && target.fromFactory {
		return target.Ref()
	}
	return AgentRef{Name: h.Name, Version: h.Version}
}

// NewFactoryAgent builds ref and every handoff target it reaches, applying configure to each copy.
func NewFactoryAgent(ctx context.Context, factory AgentFactory, ref AgentRef, configure func(*AgentOptions)) (*Agent, error) {
	return newFactoryAgent(ctx, factory, ref, configure, map[AgentRef]*Agent{})
}

func newFactoryAgent(ctx context.Context, factory AgentFactory, ref AgentRef, configure func(*AgentOptions), built map[AgentRef]*Agent) (*Agent, error) {
	if agent, ok := built[ref]; ok {
		return agent, nil
	}
	stored, err := BuildFactoryOptions(ctx, factory, ref)
	if err != nil {
		return nil, err
	}

	// A copy, since NewAgent writes to its options and the factory may share them.
	options := *stored
	options.Name = ref.Name
	options.Version = ref.Version
	options.Handoffs = nil
	if configure != nil {
		configure(&options)
	}
	agent := NewAgent(&options)
	agent.fromFactory = true
	built[ref] = agent

	for _, h := range stored.Handoffs {
		if h == nil {
			continue
		}
		if h.Agent != nil {
			agent.AddHandoffs(h)
			continue
		}
		target, err := newFactoryAgent(ctx, factory, HandoffRef(h), configure, built)
		if errors.Is(err, ErrAgentNotFound) {
			slog.WarnContext(ctx, "handoff target not found; the edge is dropped",
				slog.String("agent", ref.String()), slog.String("target", HandoffRef(h).String()))
			continue
		}
		if err != nil {
			return nil, err
		}
		agent.AddHandoffs(&Handoff{Name: h.Name, Description: h.Description, Version: h.Version, Agent: target})
	}
	return agent, nil
}

// AgentSpec is the serializable shape of a factory agent, fixed when a durable run starts.
type AgentSpec struct {
	Ref                AgentRef             `json:"ref"`
	Tools              []ToolSpec           `json:"tools,omitempty"`
	Parameters         responses.Parameters `json:"parameters"`
	Output             map[string]any       `json:"output,omitempty"`
	MaxLoops           *int                 `json:"max_loops,omitempty"`
	StickyHandoff      bool                 `json:"sticky_handoff,omitempty"`
	SingleTurn         bool                 `json:"single_turn,omitempty"`
	ClientTools        ClientToolOptions    `json:"client_tools"`
	HasInstruction     bool                 `json:"has_instruction,omitempty"`
	HasSummarizer      bool                 `json:"has_summarizer,omitempty"`
	HasMessageFilter   bool                 `json:"has_message_filter,omitempty"`
	HasMCP             bool                 `json:"has_mcp,omitempty"`
	HasSkills          bool                 `json:"has_skills,omitempty"`
	MessageAttribution bool                 `json:"message_attribution,omitempty"`
	SteeringNotices    bool                 `json:"steering_notices,omitempty"`
	Handoffs           []HandoffSpec        `json:"handoffs,omitempty"`
}

// ToolSpec is a tool's descriptor plus whether it starts background tasks.
type ToolSpec struct {
	Tool       BaseTool `json:"tool"`
	Background bool     `json:"background,omitempty"`
}

// Key is the name the runtime uses to find the tool again on the worker.
func (s ToolSpec) Key() string { return DescriptorKey(&s.Tool) }

// HandoffSpec is a handoff edge by ref.
type HandoffSpec struct {
	Ref         AgentRef `json:"ref"`
	Description string   `json:"description,omitempty"`
}

// ToolKey names a tool for routing: its function name, or the provider tool's kind.
func ToolKey(tool Tool) string { return DescriptorKey(toolDescriptor(tool)) }

// DescriptorKey is ToolKey for a descriptor.
func DescriptorKey(descriptor *BaseTool) string {
	if descriptor == nil {
		return ""
	}
	switch {
	case descriptor.ToolUnion.OfFunction != nil:
		return descriptor.ToolUnion.OfFunction.Name
	case descriptor.ToolUnion.OfWebSearch != nil:
		return "web_search"
	case descriptor.ToolUnion.OfImageGeneration != nil:
		return "image_generation"
	}
	return ""
}

// FindToolByKey returns the tool ToolKey names, or nil.
func FindToolByKey(tools []Tool, key string) Tool {
	for _, tool := range tools {
		if key != "" && ToolKey(tool) == key {
			return tool
		}
	}
	return nil
}

// DescribeAgent projects built options onto the spec a durable workflow builds its proxy from.
func DescribeAgent(ref AgentRef, options *AgentOptions) *AgentSpec {
	spec := &AgentSpec{
		Ref:            ref,
		Parameters:     options.Parameters,
		Output:         options.Output,
		MaxLoops:       options.MaxLoops,
		StickyHandoff:  options.StickyHandoff,
		SingleTurn:     options.SingleTurn,
		ClientTools:    options.ClientTools,
		HasInstruction: options.Instruction != nil,
		HasMCP:         options.MCPClient != nil,
		HasSkills:      options.SkillClient != nil,
		// The default manager steers; a factory that leaves History nil gets that.
		SteeringNotices: true,
	}
	if h := options.History; h != nil {
		spec.HasSummarizer = h.Summarizer != nil
		spec.HasMessageFilter = h.MessageFilter != nil
		spec.MessageAttribution = h.MessageAttribution
		spec.SteeringNotices = h.SteeringNotices
	}
	for _, tool := range options.Tools {
		descriptor := toolDescriptor(tool)
		if descriptor == nil {
			continue
		}
		_, background := tool.(BackgroundTool)
		spec.Tools = append(spec.Tools, ToolSpec{Tool: *descriptor, Background: background})
	}
	for _, h := range options.Handoffs {
		if h == nil {
			continue
		}
		spec.Handoffs = append(spec.Handoffs, HandoffSpec{Ref: HandoffRef(h), Description: h.Description})
	}
	return spec
}

// DescribeAgentGraph describes ref and every handoff target it reaches, ref first.
// Targets the factory cannot find are dropped from the edges, as static runtimes drop unregistered ones.
func DescribeAgentGraph(ctx context.Context, factory AgentFactory, ref AgentRef) ([]AgentSpec, error) {
	var specs []AgentSpec
	seen := map[AgentRef]bool{}
	queue := []AgentRef{ref}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if seen[current] {
			continue
		}
		seen[current] = true
		options, err := BuildFactoryOptions(ctx, factory, current)
		if err != nil {
			return nil, err
		}
		spec := DescribeAgent(current, options)
		kept := spec.Handoffs[:0:0]
		for _, h := range spec.Handoffs {
			if seen[h.Ref] || slices.Contains(queue, h.Ref) {
				kept = append(kept, h)
				continue
			}
			if _, err := BuildFactoryOptions(ctx, factory, h.Ref); errors.Is(err, ErrAgentNotFound) {
				slog.WarnContext(ctx, "handoff target not found; the edge will not exist in durable runs",
					slog.String("agent", current.String()), slog.String("target", h.Ref.String()))
				continue
			} else if err != nil {
				return nil, err
			}
			kept = append(kept, h)
			queue = append(queue, h.Ref)
		}
		spec.Handoffs = kept
		specs = append(specs, *spec)
	}
	return specs, nil
}
