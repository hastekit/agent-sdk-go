// Package agentconfig defines agents as data: a serializable Config stored per
// namespace, and a Builder that turns one into a plain agents.Agent from
// dependencies held in code.
//
// The empty namespace holds built-in agents, which every namespace can run and
// none can replace. Any other namespace — a user or a tenant — holds the agents
// it created at run time, which only it can see or run. This mirrors the skills
// and mcpclient stores, whose empty namespaces hold their globals.
//
// A config names what the agent may use; it never carries the thing itself. A
// model is an id the Builder allowlists, a tool is a name in the Builder's
// catalog, skills and MCP connectors are picks from the Builder's unfiltered
// stores. So whoever writes configs can choose among what the server offers and
// reach nothing else.
//
// Durable runtimes run a config through one generic workflow whose input is a
// Snapshot of it, so an agent created after the worker started runs durably
// without registering anything.
package agentconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"

	"github.com/hastekit/agent-sdk-go/pkg/agents/mcpclient"
	"github.com/hastekit/agent-sdk-go/pkg/agents/tools"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

var (
	// ErrNotFound reports a name the namespace does not hold.
	ErrNotFound = errors.New("agent config not found")
	// ErrInvalid marks a configuration that can never run as written: a
	// malformed config, or one naming a model, tool or handoff the server does
	// not offer. Durable runtimes treat it as permanent and do not retry it.
	ErrInvalid = errors.New("invalid agent config")
	// ErrReadOnly reports a write to a store, or a name, that cannot be written.
	ErrReadOnly = errors.New("agent config is read-only")
)

// MaxConfigBytes bounds one config's JSON encoding. A config travels in every
// durable run's input, so it is kept to the size of a long system prompt.
const MaxConfigBytes = 256 << 10

// Config is an agent as data. Every field is serializable, and every reference
// is by name, resolved by the Builder against what the server allowlists.
type Config struct {
	// Name identifies the agent within its namespace. It is also the name the
	// agent runs under, so it appears in history and in handoffs.
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`

	// Model is a model id, such as "OpenAI/gpt-4o-mini". The Builder runs only
	// ids its ModelResolver allows.
	Model      string               `json:"model"`
	Parameters responses.Parameters `json:"parameters,omitempty"`

	// Instruction is the system prompt, rendered with the Builder's prompt
	// resolvers (by default prompts.DefaultResolvers, so {{ templates }} and the
	// skills, handoffs and connectors sections all apply). Empty uses
	// DefaultInstruction.
	Instruction string `json:"instruction,omitempty"`

	// MaxLoops bounds the agent loop; zero uses the agents package default.
	MaxLoops int `json:"max_loops,omitempty"`

	// Output is a JSON schema object the final answer must follow.
	Output json.RawMessage `json:"output,omitempty"`

	// Skills names the skills the agent may read, global or the namespace's
	// own, as skills.Filter does. A name no store lists is ignored, since
	// skills come and go at run time.
	Skills []string `json:"skills,omitempty"`

	// MCP picks connectors by name, each narrowed to the tools its ToolFilter
	// allows (an empty filter keeps every tool), as mcpclient.Filter does.
	MCP map[string]mcpclient.ToolFilter `json:"mcp,omitempty"`

	// Tools names tools in the Builder's catalog.
	Tools []string `json:"tools,omitempty"`

	// Handoffs names agents this one can transfer to. A built-in agent hands
	// off among built-ins only; a namespace's agent reaches built-ins and the
	// namespace's own.
	Handoffs []Handoff `json:"handoffs,omitempty"`

	// StickyHandoff resumes a thread in the specialist its last turn ended in.
	StickyHandoff bool `json:"sticky_handoff,omitempty"`

	// SubAgents are agents this one can call as tools. Unlike a handoff, the
	// conversation stays with this agent: the model sends the sub-agent a
	// message, and the sub-agent's answer comes back as the tool's result.
	// Targets resolve as handoffs do.
	SubAgents []SubAgent `json:"sub_agents,omitempty"`
}

// Handoff is one edge of the handoff graph, by target name.
type Handoff struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// SubAgent offers another agent to this one as a tool, named after it (see
// ToolName). A call runs the sub-agent in the caller's namespace, in a
// conversation of its own under SubAgentGroupID, so it can use the
// namespace's skills and connectors and never shows among its chats. It does
// not see the caller's conversation, attachments or sandbox: the model tells
// it what it needs.
type SubAgent struct {
	// Name is the sub-agent's name.
	Name string `json:"name"`
	// Description tells the model when to call it. Empty uses the
	// sub-agent's own description.
	Description string `json:"description,omitempty"`
	// ContextMode is tools.SubAgentContextModeIsolated, the default, which
	// keeps one conversation with the sub-agent per thread of the caller's,
	// so it remembers earlier calls; or tools.SubAgentContextModeNone, which
	// tells the model each conversation's thread ID and lets it pass one back
	// to continue it, or none to start afresh.
	ContextMode tools.SubAgentContextMode `json:"context_mode,omitempty"`
}

// SubAgentGroupID is the history group sub-agents' conversations are kept in,
// apart from the namespace's chats (history.DefaultGroupID).
const SubAgentGroupID = "sub-agent"

// ToolName is the tool the model calls the sub-agent by: its name, with any
// character a tool name cannot hold replaced by an underscore.
func (s SubAgent) ToolName() string {
	var b strings.Builder
	for _, r := range s.Name {
		if b.Len() == 64 {
			break
		}
		if r == '_' || r == '-' || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// contextMode is the sub-agent's context mode, defaulted.
func (s SubAgent) contextMode() tools.SubAgentContextMode {
	if s.ContextMode == "" {
		return tools.SubAgentContextModeIsolated
	}
	return s.ContextMode
}

// DefaultInstruction is the system prompt of a config that leaves it empty.
const DefaultInstruction = "You are a helpful assistant."

// Validate checks a config's own shape: names, sizes and duplicates. Whether
// its model, tools and handoff targets exist is the Builder's to say — see
// Builder.Validate — since only the server knows what it offers.
func Validate(c Config) error {
	if !validName(c.Name) {
		return fmt.Errorf("%w: name %q", ErrInvalid, c.Name)
	}
	if strings.TrimSpace(c.Model) == "" {
		return fmt.Errorf("%w: %s: model is required", ErrInvalid, c.Name)
	}
	if c.MaxLoops < 0 {
		return fmt.Errorf("%w: %s: max_loops must not be negative", ErrInvalid, c.Name)
	}
	if err := uniqueNames("skill", c.Skills); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrInvalid, c.Name, err)
	}
	if err := uniqueNames("tool", c.Tools); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrInvalid, c.Name, err)
	}
	for name := range c.MCP {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("%w: %s: blank MCP connector name", ErrInvalid, c.Name)
		}
	}
	targets := make([]string, 0, len(c.Handoffs))
	for _, h := range c.Handoffs {
		if !validName(h.Name) {
			return fmt.Errorf("%w: %s: handoff name %q", ErrInvalid, c.Name, h.Name)
		}
		if h.Name == c.Name {
			return fmt.Errorf("%w: %s: an agent cannot hand off to itself", ErrInvalid, c.Name)
		}
		targets = append(targets, h.Name)
	}
	if err := uniqueNames("handoff", targets); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrInvalid, c.Name, err)
	}
	// Sub-agents are tools, so their names must not clash with each other's or
	// the tools the config picks.
	subAgents := make([]string, 0, len(c.SubAgents))
	toolNames := slices.Clone(c.Tools)
	for _, s := range c.SubAgents {
		if !validName(s.Name) {
			return fmt.Errorf("%w: %s: sub-agent name %q", ErrInvalid, c.Name, s.Name)
		}
		if s.Name == c.Name {
			return fmt.Errorf("%w: %s: an agent cannot be its own sub-agent", ErrInvalid, c.Name)
		}
		if s.ContextMode != "" && s.ContextMode != tools.SubAgentContextModeIsolated && s.ContextMode != tools.SubAgentContextModeNone {
			return fmt.Errorf("%w: %s: sub-agent %q: context_mode must be %q or %q", ErrInvalid, c.Name, s.Name, tools.SubAgentContextModeIsolated, tools.SubAgentContextModeNone)
		}
		subAgents = append(subAgents, s.Name)
		toolNames = append(toolNames, s.ToolName())
	}
	if err := uniqueNames("sub-agent", subAgents); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrInvalid, c.Name, err)
	}
	if err := uniqueNames("tool", toolNames); err != nil {
		return fmt.Errorf("%w: %s: %v (a sub-agent is a tool named after it)", ErrInvalid, c.Name, err)
	}
	if _, err := c.OutputSchema(); err != nil {
		return err
	}
	data, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrInvalid, c.Name, err)
	}
	if len(data) > MaxConfigBytes {
		return fmt.Errorf("%w: %s: config exceeds %d bytes", ErrInvalid, c.Name, MaxConfigBytes)
	}
	return nil
}

// OutputSchema decodes Output for agents.AgentOptions.Output, which takes the
// schema as a map. It is nil when the config sets no schema, and an error
// wrapping ErrInvalid when Output is not a JSON object.
func (c Config) OutputSchema() (map[string]any, error) {
	if len(c.Output) == 0 || string(c.Output) == "null" {
		return nil, nil
	}
	var schema map[string]any
	if err := json.Unmarshal(c.Output, &schema); err != nil || schema == nil {
		return nil, fmt.Errorf("%w: %s: output must be a JSON schema object", ErrInvalid, c.Name)
	}
	return schema, nil
}

func uniqueNames(kind string, names []string) error {
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("blank %s name", kind)
		}
		if seen[name] {
			return fmt.Errorf("duplicate %s %q", kind, name)
		}
		seen[name] = true
	}
	return nil
}

// validName accepts what the skills store accepts, so one name is valid in a
// URL path segment and as a storage key alike.
func validName(s string) bool {
	return s != "" && len(s) <= 128 && s == strings.TrimSpace(s) && fs.ValidPath(s) &&
		!strings.ContainsAny(s, "/\\\x00") && s != "."
}

// clone deep-copies a config through its JSON form, which is the form every
// store keeps and every runtime carries, so nothing a caller holds can change
// what was stored.
func clone(c Config) (Config, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return Config{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return decode(data)
}

func decode(data []byte) (Config, error) {
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return c, nil
}

// Snapshot is a config graph frozen for one run: the agent the run enters at
// and every agent reachable from it through handoffs, as they were when the
// run started. Durable runtimes take it as their workflow input, so replay sees
// the same agents however the store changes meanwhile; an edit applies from
// the next run.
type Snapshot struct {
	// Namespace is the scope the graph was resolved in. A run may only use a
	// snapshot resolved for its own namespace.
	Namespace string `json:"namespace"`
	// Root names the agent the run enters at.
	Root string `json:"root"`
	// Agents holds every config in the graph by name, the root included.
	Agents map[string]Config `json:"agents"`
}

// Check reports whether the snapshot can run in namespace: well formed, its
// root present, and resolved for that namespace. Durable runtimes call it
// before building anything, so a snapshot from one namespace never runs
// against another's data.
func (s *Snapshot) Check(namespace string) error {
	if s == nil {
		return fmt.Errorf("%w: no agent snapshot", ErrInvalid)
	}
	if s.Namespace != namespace {
		return fmt.Errorf("%w: agent snapshot resolved for namespace %q cannot run in %q", ErrInvalid, s.Namespace, namespace)
	}
	root, ok := s.Agents[s.Root]
	if !ok || root.Name != s.Root {
		return fmt.Errorf("%w: agent snapshot is missing its root %q", ErrInvalid, s.Root)
	}
	for _, name := range slices.Sorted(maps.Keys(s.Agents)) {
		cfg := s.Agents[name]
		if cfg.Name != name {
			return fmt.Errorf("%w: agent snapshot holds %q under %q", ErrInvalid, cfg.Name, name)
		}
		if err := Validate(cfg); err != nil {
			return err
		}
	}
	return nil
}

// Sub is the part of the snapshot a run entering at name uses: name and every
// agent reachable from it, frozen as they are here. A sub-agent call runs it.
func (s *Snapshot) Sub(name string) *Snapshot {
	sub := &Snapshot{Namespace: s.Namespace, Root: name, Agents: map[string]Config{}}
	queue := []string{name}
	for len(queue) > 0 {
		cfg, ok := s.Agents[queue[0]]
		queue = queue[1:]
		if _, done := sub.Agents[cfg.Name]; !ok || done {
			continue
		}
		sub.Agents[cfg.Name] = cfg
		for _, h := range cfg.Handoffs {
			queue = append(queue, h.Name)
		}
		for _, a := range cfg.SubAgents {
			queue = append(queue, a.Name)
		}
	}
	return sub
}

// fingerprint identifies a snapshot's content, so a cache never serves an agent
// built from an older version of a config.
func (s *Snapshot) fingerprint() (string, error) {
	data, err := json.Marshal(s) // map keys encode sorted, so equal snapshots encode equally
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
