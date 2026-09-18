package workflow

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
)

// Node IDs must match their Graph.AddNode key. Configurations are copied at
// construction; supplied agents, connectors, and HTTP clients remain shared.
// Expression fields accept the same whole-scalar ${{ ... }} bindings as YAML.
type JavaScriptNodeConfig struct {
	Code    string        `yaml:"code"`
	Timeout time.Duration `yaml:"-"` // zero: one second
}
type IfElseNodeConfig struct {
	Condition string        `yaml:"condition"` // JavaScript expression returning a boolean
	Timeout   time.Duration `yaml:"-"`
}
type SwitchCase struct {
	Value any    `yaml:"value"` // literal JSON value to compare
	Port  string `yaml:"port"`
}
type SwitchNodeConfig struct {
	Value   any           `yaml:"value"`
	Cases   []SwitchCase  `yaml:"cases"`
	Timeout time.Duration `yaml:"-"`
}
type DelayNodeConfig struct {
	Duration time.Duration `yaml:"duration"`
}
type APINodeConfig struct {
	URL               string            `yaml:"url"`
	Method            string            `yaml:"method"` // zero: GET
	Headers           map[string]string `yaml:"headers"`
	Body              any               `yaml:"body"`    // JSON value, optionally containing expressions
	Timeout           time.Duration     `yaml:"timeout"` // zero: 30 seconds
	HTTPClient        *http.Client      `yaml:"-"`
	MaxResponseBytes  int64             `yaml:"-"` // zero: 4 MiB
	ExpressionTimeout time.Duration     `yaml:"-"` // zero: one second
}
type HumanNodeConfig struct {
	Message string         `yaml:"message"`
	Schema  map[string]any `yaml:"schema"`
	Timeout time.Duration  `yaml:"-"` // expression timeout
}
type MCPNodeConfig struct {
	Server    agents.MCPToolset
	Tool      string
	Arguments any           // JSON object or an expression returning one
	Timeout   time.Duration // expression timeout
}
type AgentNodeConfig struct {
	Agent   AgentRunner
	Message string
	Timeout time.Duration // expression timeout
}

func newBuiltinNode(id string, kind NodeType) (*builtinNode, error) {
	if strings.TrimSpace(id) == "" || id == StartNode || id == EndNode {
		return nil, fmt.Errorf("node ID must be non-empty and not START or END")
	}
	return &builtinNode{BaseNode: BaseNode{NodeType: kind}, id: id, ports: map[string]bool{DefaultPort: true}}, nil
}
func nodeTimeout(timeout time.Duration) (time.Duration, error) {
	if timeout < 0 {
		return 0, fmt.Errorf("expression timeout must not be negative")
	}
	if timeout == 0 {
		timeout = time.Second
	}
	return timeout, nil
}
func copyBindings(value any) (any, error) {
	cloned, err := jsonValue(value)
	if err != nil {
		return nil, err
	}
	if err := validateBindings(cloned); err != nil {
		return nil, err
	}
	return cloned, nil
}
