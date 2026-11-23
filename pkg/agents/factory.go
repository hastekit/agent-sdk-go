package agents

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// AgentRef names one version of a stored agent. An empty Version asks the loader for its latest.
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

// AgentSpec is an agent as data. Durable runs carry it, so it holds names, never code or keys.
type AgentSpec struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	// Model is a "Provider/model" id, resolved on the worker by AgentFactory.Models.
	Model string `json:"model"`
	// Instruction is the system prompt, rendered with prompts.DefaultResolvers.
	Instruction string               `json:"instruction,omitempty"`
	Parameters  responses.Parameters `json:"parameters"`
	Output      map[string]any       `json:"output,omitempty"`
	MaxLoops    int                  `json:"max_loops,omitempty"`
	// Tools names tools in AgentFactory.Tools.
	Tools    []string      `json:"tools,omitempty"`
	Handoffs []HandoffSpec `json:"handoffs,omitempty"`
}

// Ref names the agent this spec describes.
func (s *AgentSpec) Ref() AgentRef { return AgentRef{Name: s.Name, Version: s.Version} }

// HandoffSpec is a handoff to another stored agent, loaded when the handoff happens.
type HandoffSpec struct {
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
}

// Ref names the handoff's target.
func (h HandoffSpec) Ref() AgentRef { return AgentRef{Name: h.Name, Version: h.Version} }

// ModelResolver turns an AgentSpec.Model id into a provider; the root package's *LLMClient is one.
type ModelResolver interface {
	ResolveModel(id string) (llm.Provider, error)
}

var (
	// ErrAgentNotFound is what Load wraps for an unknown ref; durable runtimes do not retry it.
	ErrAgentNotFound = errors.New("agent not found")
	// ErrInvalidAgentSpec marks a loaded spec this factory cannot build; durable runtimes do not retry it.
	ErrInvalidAgentSpec = errors.New("invalid agent spec")
)

// AgentFactory builds agents from stored specs. Load runs for every new run and handoff;
// the other fields hold code and keys, are fixed per worker, and never travel with a run.
type AgentFactory struct {
	// Load returns the spec stored under ref, wrapping ErrAgentNotFound for unknown refs.
	Load func(ctx context.Context, ref AgentRef) (*AgentSpec, error)
	// Models resolves each spec's Model.
	Models ModelResolver
	// Tools are the tools a spec can name, by function name.
	Tools []Tool
	// History stores every factory agent's conversations. Durable runtimes require it.
	History *history.CommonConversationManager
	// Middlewares wrap every factory agent's model, tool, history and prompt calls.
	Middlewares []Middleware
}

// Validate checks the factory can serve specs.
func (f *AgentFactory) Validate() error {
	if f == nil {
		return errors.New("agent factory is nil")
	}
	if f.Load == nil {
		return errors.New("agent factory has no Load")
	}
	if f.Models == nil {
		return errors.New("agent factory has no Models")
	}
	seen := map[string]bool{}
	for _, tool := range f.Tools {
		name := factoryToolName(tool)
		if name == "" {
			return errors.New("agent factory has a tool with no name")
		}
		if seen[name] {
			return fmt.Errorf("agent factory has two tools named %q", name)
		}
		seen[name] = true
	}
	return nil
}

// Tool returns the factory tool named name, or nil.
func (f *AgentFactory) Tool(name string) Tool {
	for _, tool := range f.Tools {
		if factoryToolName(tool) == name {
			return tool
		}
	}
	return nil
}

// LoadSpec loads ref and checks the spec is complete and names only this factory's tools.
func (f *AgentFactory) LoadSpec(ctx context.Context, ref AgentRef) (*AgentSpec, error) {
	if strings.TrimSpace(ref.Name) == "" {
		return nil, fmt.Errorf("%w: agent ref has no name", ErrInvalidAgentSpec)
	}
	spec, err := f.Load(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("load agent %s: %w", ref, err)
	}
	if spec == nil {
		return nil, fmt.Errorf("load agent %s: %w", ref, ErrAgentNotFound)
	}
	if err := f.check(ref, spec); err != nil {
		return nil, fmt.Errorf("load agent %s: %w: %s", ref, ErrInvalidAgentSpec, err)
	}
	return spec, nil
}

func (f *AgentFactory) check(ref AgentRef, spec *AgentSpec) error {
	if spec.Name != ref.Name {
		return fmt.Errorf("loader returned agent %q", spec.Name)
	}
	if ref.Version != "" && spec.Version != ref.Version {
		return fmt.Errorf("loader returned version %q", spec.Version)
	}
	if spec.Model == "" {
		return errors.New("spec has no model")
	}
	for _, name := range spec.Tools {
		if f.Tool(name) == nil {
			return fmt.Errorf("unknown tool %q", name)
		}
	}
	return nil
}

// factoryToolName is the name a spec uses for tool: its function name, or a provider tool's kind.
func factoryToolName(tool Tool) string {
	descriptor := tool.GetToolDescriptor()
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
