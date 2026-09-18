package workflow

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"gopkg.in/yaml.v3"
)

// AgentRunner is implemented by *agents.Agent. Run must honor cancellation.
type AgentRunner interface {
	Run(context.Context, *agents.AgentInput) (*agents.AgentOutput, error)
}

// Dependencies supplies trusted application resources. Descriptors refer to
// agents and MCP connectors by name; they never construct credentials or clients.
type Dependencies struct {
	Agents            map[string]AgentRunner
	MCPServers        map[string]agents.MCPToolset
	HTTPClient        *http.Client  // nil: a client with a 30-second timeout
	JavaScriptTimeout time.Duration // zero: one second per expression/code execution
	MaxResponseBytes  int64         // zero: 4 MiB per API response
}

// Descriptor is a versioned, declarative workflow graph.
type Descriptor struct {
	Version int        `yaml:"version"`
	ID      string     `yaml:"id"`
	Nodes   []NodeSpec `yaml:"nodes"`
	Edges   []EdgeSpec `yaml:"edges"`
}
type NodeSpec struct {
	ID     string    `yaml:"id"`
	Type   NodeType  `yaml:"type"`
	Config yaml.Node `yaml:"config"`
}
type EdgeSpec struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
	Port string `yaml:"port,omitempty"`
}

// LoadYAML strictly decodes one document and compiles it using the existing
// graph engine. Unknown fields, missing dependencies and invalid graphs fail here.
func LoadYAML(data []byte, deps Dependencies) (*Compiled, error) {
	var descriptor Descriptor
	if err := decodeYAML(data, &descriptor); err != nil {
		return nil, fmt.Errorf("workflow YAML: %w", err)
	}
	return descriptor.Compile(deps)
}
func decodeYAML(data []byte, target any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err != nil {
			return err
		}
		return fmt.Errorf("expected a single YAML document")
	}
	return nil
}
func nodeConfig(spec NodeSpec, target any) error {
	data, err := yaml.Marshal(&spec.Config)
	if err != nil {
		return err
	}
	return decodeYAML(data, target)
}
func (d Descriptor) Compile(deps Dependencies) (*Compiled, error) {
	if d.Version != 1 {
		return nil, fmt.Errorf("workflow: unsupported descriptor version %d", d.Version)
	}
	if deps.JavaScriptTimeout < 0 || deps.MaxResponseBytes < 0 || deps.MaxResponseBytes == math.MaxInt64 {
		return nil, fmt.Errorf("workflow: dependency limits must not be negative")
	}
	if deps.JavaScriptTimeout == 0 {
		deps.JavaScriptTimeout = time.Second
	}
	if deps.MaxResponseBytes == 0 {
		deps.MaxResponseBytes = 4 << 20
	}
	if deps.HTTPClient == nil {
		deps.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	g := NewGraph(d.ID)
	for _, spec := range d.Nodes {
		var raw any
		if err := spec.Config.Decode(&raw); err != nil {
			return nil, fmt.Errorf("node %q config: %w", spec.ID, err)
		}
		if err := validateBindings(raw); err != nil {
			return nil, fmt.Errorf("node %q config: %w", spec.ID, err)
		}
		n, err := buildBuiltin(spec, deps)
		if err != nil {
			return nil, fmt.Errorf("workflow node %q (%s): %w", spec.ID, spec.Type, err)
		}
		g.AddNode(spec.ID, n)
	}
	for _, edge := range d.Edges {
		port := edge.Port
		if port == "" {
			port = DefaultPort
		}
		g.AddEdgeOnPort(edge.From, port, edge.To)
	}
	return g.Compile()
}

// YAML only decodes configuration and resolves named dependencies. All node
// construction and validation lives in the public Go constructors.
func buildBuiltin(spec NodeSpec, deps Dependencies) (Node, error) {
	switch spec.Type {
	case "javascript":
		var cfg JavaScriptNodeConfig
		if err := nodeConfig(spec, &cfg); err != nil {
			return nil, err
		}
		cfg.Timeout = deps.JavaScriptTimeout
		return NewJavaScriptNode(spec.ID, cfg)
	case "if_else":
		var cfg IfElseNodeConfig
		if err := nodeConfig(spec, &cfg); err != nil {
			return nil, err
		}
		cfg.Timeout = deps.JavaScriptTimeout
		return NewIfElseNode(spec.ID, cfg)
	case "switch":
		var cfg SwitchNodeConfig
		if err := nodeConfig(spec, &cfg); err != nil {
			return nil, err
		}
		cfg.Timeout = deps.JavaScriptTimeout
		return NewSwitchNode(spec.ID, cfg)
	case "delay":
		var cfg struct {
			Duration *time.Duration `yaml:"duration"`
		}
		if err := nodeConfig(spec, &cfg); err != nil {
			return nil, err
		}
		if cfg.Duration == nil {
			return nil, fmt.Errorf("duration is required")
		}
		return NewDelayNode(spec.ID, DelayNodeConfig{Duration: *cfg.Duration})
	case "api":
		var cfg APINodeConfig
		if err := nodeConfig(spec, &cfg); err != nil {
			return nil, err
		}
		cfg.HTTPClient = deps.HTTPClient
		cfg.ExpressionTimeout = deps.JavaScriptTimeout
		cfg.MaxResponseBytes = deps.MaxResponseBytes
		return NewAPINode(spec.ID, cfg)
	case "human":
		var cfg HumanNodeConfig
		if err := nodeConfig(spec, &cfg); err != nil {
			return nil, err
		}
		cfg.Timeout = deps.JavaScriptTimeout
		return NewHumanNode(spec.ID, cfg)
	case "mcp":
		var cfg struct {
			Server    string `yaml:"server"`
			Tool      string `yaml:"tool"`
			Arguments any    `yaml:"arguments"`
		}
		if err := nodeConfig(spec, &cfg); err != nil {
			return nil, err
		}
		return NewMCPNode(spec.ID, MCPNodeConfig{Server: deps.MCPServers[cfg.Server], Tool: cfg.Tool, Arguments: cfg.Arguments, Timeout: deps.JavaScriptTimeout})
	case "agent":
		var cfg struct {
			Agent   string `yaml:"agent"`
			Message string `yaml:"message"`
		}
		if err := nodeConfig(spec, &cfg); err != nil {
			return nil, err
		}
		return NewAgentNode(spec.ID, AgentNodeConfig{Agent: deps.Agents[cfg.Agent], Message: cfg.Message, Timeout: deps.JavaScriptTimeout})
	default:
		return nil, fmt.Errorf("unknown node type %q", spec.Type)
	}
}
