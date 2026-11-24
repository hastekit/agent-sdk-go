package agentconfig

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"sync"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/mcpclient"
	"github.com/hastekit/agent-sdk-go/pkg/agents/prompts"
	"github.com/hastekit/agent-sdk-go/pkg/agents/skills"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/agents/tools"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
)

// ModelResolver turns a config's model id into a provider, and is the
// allowlist: an id it does not offer is refused with an error wrapping
// ErrInvalid, so a config naming it never runs.
type ModelResolver interface {
	ResolveModel(id string) (llm.Provider, error)
}

// Models is a ModelResolver over a fixed set of models, each bound once at
// setup:
//
//	agentconfig.Models{
//		"OpenAI/gpt-4o-mini": client.Model("OpenAI/gpt-4o-mini"),
//		"Anthropic/claude-sonnet-4-5": client.Model("Anthropic/claude-sonnet-4-5"),
//	}
type Models map[string]llm.Provider

func (m Models) ResolveModel(id string) (llm.Provider, error) {
	if provider := m[id]; provider != nil {
		return provider, nil
	}
	return nil, fmt.Errorf("%w: model %q is not allowed", ErrInvalid, id)
}

// AllowModels is a ModelResolver for the allowed ids, binding each with resolve
// the first time a config uses it and reusing that provider afterwards:
//
//	agentconfig.AllowModels(func(id string) (llm.Provider, error) {
//		return client.Model(id), nil
//	}, "OpenAI/gpt-4o-mini", "OpenAI/gpt-4o")
func AllowModels(resolve func(id string) (llm.Provider, error), allowed ...string) ModelResolver {
	m := &allowedModels{resolve: resolve, allowed: map[string]bool{}, bound: map[string]llm.Provider{}}
	for _, id := range allowed {
		m.allowed[id] = true
	}
	return m
}

type allowedModels struct {
	resolve func(string) (llm.Provider, error)
	allowed map[string]bool
	mu      sync.Mutex
	bound   map[string]llm.Provider
}

func (m *allowedModels) ResolveModel(id string) (llm.Provider, error) {
	if !m.allowed[id] || m.resolve == nil {
		return nil, fmt.Errorf("%w: model %q is not allowed", ErrInvalid, id)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if provider := m.bound[id]; provider != nil {
		return provider, nil
	}
	provider, err := m.resolve(id)
	if err != nil {
		return nil, err
	}
	if provider == nil {
		return nil, fmt.Errorf("%w: model %q resolved to no provider", ErrInvalid, id)
	}
	m.bound[id] = provider
	return provider, nil
}

// Runtime runs config-built agents durably, through one generic workflow that
// takes a Snapshot as input. sdk.TemporalRuntime and sdk.RestateRuntime
// implement it. The Builder binds each agent it builds to the runtime's run
// for that agent's snapshot, through the plain agents.AgentOptions.Runtime
// field.
type Runtime interface {
	// StreamBroker is the broker the workflows publish on, which agents built
	// for this runtime must stream from.
	StreamBroker() agents.StreamBroker
	// Bind returns the runtime that runs snapshot.
	Bind(snapshot *Snapshot) agents.Runtime
}

// Dependencies are what configs choose among, held in code. Only Models is
// required. Where configs are stored is the Registry's concern, not the
// Builder's: a Builder only builds the snapshots it is handed, so durable
// workers, which take each run's snapshot as input, never read a store.
type Dependencies struct {
	// Models allowlists the model ids configs may name.
	Models ModelResolver

	// BuiltInSkillStore and UserSkillStore are the unfiltered stores configs pick
	// skills from: the developer's, and each namespace's own, as a
	// skills.Client takes them. Each agent sees only its picks, through
	// skills.Filter.
	BuiltInSkillStore skills.Store
	UserSkillStore    skills.Store

	// BuiltInMCPStore and UserMCPStore are the unfiltered stores configs pick
	// MCP servers from, as an mcpclient.Client takes them. Each agent sees only its picks,
	// through mcpclient.Filter. MCPCredentials and MCPSchemaCache are shared by
	// every agent's client.
	BuiltInMCPStore mcpclient.Store
	UserMCPStore    mcpclient.Store
	MCPCredentials  mcpclient.CredentialProvider
	MCPSchemaCache  mcpclient.SchemaCache

	// Tools is the catalog configs pick tools from, by key. A catalog tool is
	// shared by every agent that names it, so it must be safe for concurrent
	// calls. A catalog tool may be an agents.BackgroundTool: locally its task
	// is waited on in process; durable runtimes wait in a workflow or service
	// of their own, found by the same key, and deliver into the task's
	// snapshot.
	Tools map[string]agents.Tool

	// History persists every config-built agent's conversations. Nil keeps
	// them in memory, shared by every agent the Builder builds.
	History *history.CommonConversationManager

	// Middlewares wrap every config-built agent's model, tool, history and
	// prompt calls.
	Middlewares []agents.Middleware

	// ClientTools configures how runs wait for tools the client runs itself.
	ClientTools agents.ClientToolOptions

	// PromptOptions render every Instruction. Nil uses
	// prompts.WithResolver(prompts.DefaultResolvers()...).
	PromptOptions []prompts.PromptOption

	// StreamBroker is the broker local runs stream on. A durable Runtime
	// supplies its own; nil without one uses an in-memory broker.
	StreamBroker agents.StreamBroker

	// Runtime runs built agents durably. Nil runs them in process.
	Runtime Runtime
}

// Builder turns snapshots into plain agents.Agent values from code-side
// Dependencies. It looks nothing up by name: a Registry resolves the snapshot
// and hands it over. Building is plain construction, so nothing is cached. It
// is safe for concurrent use.
//
// Durable workers use the same Builder to rebuild each dependency from the
// part of a config an activity carries — see Model, Tool, Prompt, SkillClient
// and MCPClient — so the worker and the caller must be given equal
// Dependencies.
type Builder struct {
	deps    Dependencies
	broker  agents.StreamBroker
	history *history.CommonConversationManager
}

// NewBuilder validates the dependencies and copies the slices and maps it is
// given, so later changes to them do not apply.
func NewBuilder(deps Dependencies) (*Builder, error) {
	if deps.Models == nil {
		return nil, errors.New("agentconfig: a ModelResolver is required")
	}
	deps.Middlewares = slices.Clone(deps.Middlewares)
	deps.PromptOptions = slices.Clone(deps.PromptOptions)
	deps.Tools = maps.Clone(deps.Tools)
	for name, tool := range deps.Tools {
		if tool == nil || tool.GetToolDescriptor() == nil {
			return nil, fmt.Errorf("agentconfig: catalog tool %q has no descriptor", name)
		}
	}

	broker := deps.StreamBroker
	if deps.Runtime != nil {
		runtimeBroker := deps.Runtime.StreamBroker()
		if runtimeBroker == nil {
			return nil, errors.New("agentconfig: the runtime has no stream broker")
		}
		if broker != nil && broker != runtimeBroker {
			return nil, errors.New("agentconfig: StreamBroker must be the runtime's broker")
		}
		broker = runtimeBroker
	}
	if broker == nil {
		broker = streambroker.NewMemoryStreamBroker()
	}

	historyManager := deps.History
	if historyManager == nil {
		historyManager = history.NewConversationManager(history.NewInMemoryConversationPersistence())
	}

	return &Builder{deps: deps, broker: broker, history: historyManager}, nil
}

// StreamBroker is the broker built agents stream on.
func (b *Builder) StreamBroker() agents.StreamBroker { return b.broker }

// History persists every built agent's conversations.
func (b *Builder) History() *history.CommonConversationManager { return b.history }

// Middlewares wrap every built agent's calls.
func (b *Builder) Middlewares() []agents.Middleware { return slices.Clone(b.deps.Middlewares) }

// ClientTools configures client-run tools for every built agent.
func (b *Builder) ClientTools() agents.ClientToolOptions { return b.deps.ClientTools }

// Build turns a snapshot into a plain agent: agents.NewAgent for every config
// in the graph, handoffs wired between them, the root bound to the durable
// Runtime when there is one.
func (b *Builder) Build(snapshot *Snapshot) (*agents.Agent, error) {
	if err := snapshot.Check(snapshot.Namespace); err != nil {
		return nil, err
	}

	built := make(map[string]*agents.Agent, len(snapshot.Agents))
	for _, name := range slices.Sorted(maps.Keys(snapshot.Agents)) {
		options, err := b.AgentOptions(snapshot.Agents[name])
		if err != nil {
			return nil, err
		}
		options.Tools = append(options.Tools, b.SubAgentTools(snapshot, name)...)
		if name == snapshot.Root && b.deps.Runtime != nil {
			options.Runtime = b.deps.Runtime.Bind(snapshot)
		}
		built[name] = agents.NewAgent(options)
	}
	// Edges after every node exists, so a cycle — a specialist that hands
	// back — wires like any other edge.
	for _, name := range slices.Sorted(maps.Keys(snapshot.Agents)) {
		for _, h := range snapshot.Agents[name].Handoffs {
			if target := built[h.Name]; target != nil {
				built[name].AddHandoffs(agents.NewHandoff(h.Name, h.Description, target))
			}
		}
	}
	return built[snapshot.Root], nil
}

// SubAgentTools are the tools that call the snapshot's agent name's sub-agents,
// in the config's order, each a tools.AgentTool named by SubAgent.ToolName.
// A call builds the sub-agent from the snapshot as it was frozen (see
// Snapshot.Sub) and runs it in the snapshot's namespace, in the
// SubAgentGroupID history group, on the Builder's Runtime when there is one:
// durably, in a run of its own. Durable runtimes that run tools in steps of
// their own build these there; nothing here does I/O until a call.
func (b *Builder) SubAgentTools(snapshot *Snapshot, name string) []agents.Tool {
	var result []agents.Tool
	for _, sub := range snapshot.Agents[name].SubAgents {
		target, ok := snapshot.Agents[sub.Name]
		if !ok {
			continue
		}
		description := sub.Description
		if description == "" {
			description = target.Description
		}
		if description == "" {
			description = "Ask " + target.Name + " to help with part of the task."
		}
		shell := agents.NewAgent(&agents.AgentOptions{
			Name:    target.Name,
			Runtime: &subAgentRun{builder: b, snapshot: snapshot.Sub(target.Name)},
		})
		result = append(result, tools.NewAgentTool(sub.ToolName(), description, shell, sub.contextMode(), tools.WithoutTracing))
	}
	return result
}

// subAgentRun is the runtime of a sub-agent tool's shell agent: it runs the
// real sub-agent in its place.
//
// tools.AgentTool runs its agent in a namespace of its own beneath the
// caller's. A config-built sub-agent runs in the caller's instead, where its
// picks of the namespace's skills and connectors are, and which its snapshot
// was resolved for. Its conversations are its own — not the caller's
// session, which a thread's history group follows — so they go to
// SubAgentGroupID, apart from the namespace's chats. Like a sub-agent in a
// namespace of its own, it does not share the caller's attachments or sandbox.
type subAgentRun struct {
	builder  *Builder
	snapshot *Snapshot
}

func (s *subAgentRun) RegisterAgent(*agents.AgentOptions) error { return nil }
func (s *subAgentRun) StreamBroker() agents.StreamBroker        { return s.builder.broker }

func (s *subAgentRun) Run(ctx context.Context, _ *agents.Agent, in *agents.AgentInput) (*agents.AgentOutput, error) {
	agent, err := s.builder.Build(s.snapshot)
	if err != nil {
		return nil, err
	}
	run := *in
	run.Namespace = s.snapshot.Namespace
	run.GroupID = SubAgentGroupID
	run.SessionID = "" // a conversation of its own, in its own group
	return agent.ExecuteWithoutTrace(ctx, &run)
}

// AgentOptions resolves one config's dependencies into plain options, with no
// handoffs or sub-agents: wiring those needs the whole graph. Durable runtimes
// that wrap real dependencies in their own proxies, as Restate's does, start
// from these.
func (b *Builder) AgentOptions(cfg Config) (*agents.AgentOptions, error) {
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	provider, err := b.Model(cfg.Model)
	if err != nil {
		return nil, err
	}
	tools := make([]agents.Tool, 0, len(cfg.Tools))
	for _, name := range cfg.Tools {
		tool, err := b.Tool(name)
		if err != nil {
			return nil, err
		}
		tools = append(tools, tool)
	}
	copy, err := clone(cfg) // the agent keeps its own parameters
	if err != nil {
		return nil, err
	}
	output, err := cfg.OutputSchema()
	if err != nil {
		return nil, err
	}
	options := &agents.AgentOptions{
		Name:          cfg.Name,
		LLM:           provider,
		Instruction:   b.Prompt(cfg.Instruction),
		Parameters:    copy.Parameters,
		Output:        output,
		Tools:         tools,
		History:       b.history,
		Middlewares:   b.Middlewares(),
		ClientTools:   b.deps.ClientTools,
		StickyHandoff: cfg.StickyHandoff,
		StreamBroker:  b.broker,
	}
	if cfg.MaxLoops > 0 {
		options.MaxLoops = &copy.MaxLoops
	}
	if client := b.SkillClient(cfg.Skills); client != nil {
		options.SkillClient = client
	}
	if client := b.MCPClient(cfg.MCP); client != nil {
		options.MCPClient = client
	}
	return options, nil
}

// Validate checks that cfg can run here: its shape, its model on the
// allowlist and its tools in the catalog. Skills and MCP picks are not
// checked, since those catalogs change at run time and an absent pick is
// ignored; nor are handoff targets, which may be created later.
func (b *Builder) Validate(cfg Config) error {
	if err := Validate(cfg); err != nil {
		return err
	}
	if _, err := b.Model(cfg.Model); err != nil {
		return err
	}
	taken := map[string]bool{}
	for _, name := range cfg.Tools {
		tool, err := b.Tool(name)
		if err != nil {
			return err
		}
		if function := tool.GetToolDescriptor().ToolUnion.OfFunction; function != nil {
			taken[function.Name] = true
		}
	}
	for _, sub := range cfg.SubAgents {
		if taken[sub.ToolName()] {
			return fmt.Errorf("%w: %s: sub-agent %q would be a tool named %q, which a picked tool already is", ErrInvalid, cfg.Name, sub.Name, sub.ToolName())
		}
	}
	return nil
}

// Model resolves a model id through the allowlist.
func (b *Builder) Model(id string) (llm.Provider, error) {
	provider, err := b.deps.Models.ResolveModel(id)
	if err != nil {
		return nil, err
	}
	if provider == nil {
		return nil, fmt.Errorf("%w: model %q resolved to no provider", ErrInvalid, id)
	}
	return provider, nil
}

// Tool returns the catalog's tool by key.
func (b *Builder) Tool(name string) (agents.Tool, error) {
	if tool := b.deps.Tools[name]; tool != nil {
		return tool, nil
	}
	return nil, fmt.Errorf("%w: tool %q is not in the catalog", ErrInvalid, name)
}

// ToolNames lists the catalog's keys, for a UI offering them.
func (b *Builder) ToolNames() []string { return slices.Sorted(maps.Keys(b.deps.Tools)) }

// Prompt renders an instruction with the Builder's prompt options.
func (b *Builder) Prompt(instruction string) agents.SystemPromptProvider {
	if instruction == "" {
		instruction = DefaultInstruction
	}
	options := b.deps.PromptOptions
	if options == nil {
		options = []prompts.PromptOption{prompts.WithResolver(prompts.DefaultResolvers()...)}
	}
	return prompts.New(instruction, options...)
}

// SkillClient reads a config's skills: the named ones and no others, from the
// Builder's built-in and user stores alike, every one the agent's own and
// always on. A run cannot turn a pick off; to give the agent another skill,
// edit its config. It returns nil when the config picks none or there are no
// skill stores.
func (b *Builder) SkillClient(names []string) agents.SkillClient {
	if len(names) == 0 || (b.deps.BuiltInSkillStore == nil && b.deps.UserSkillStore == nil) {
		return nil
	}
	return alwaysOnSkills{client: skills.NewClient(
		skills.Filter(b.deps.BuiltInSkillStore, names),
		skills.WithUserStore(skills.Filter(b.deps.UserSkillStore, names)),
	)}
}

// alwaysOnSkills lists every skill as global: each is one of the config's
// picks, which no run turns off.
type alwaysOnSkills struct{ client *skills.Client }

func (s alwaysOnSkills) ListSkills(ctx context.Context, namespace string, runContext map[string]any) ([]agents.Skill, error) {
	listed, err := s.client.ListSkills(ctx, namespace, runContext)
	for i := range listed {
		listed[i].Global = true
	}
	return listed, err
}

func (s alwaysOnSkills) ReadSkill(ctx context.Context, namespace string, runContext map[string]any, name, file string) (string, error) {
	return s.client.ReadSkill(ctx, namespace, runContext, name, file)
}

// MCPClient reaches a config's connectors: the picked ones and no others, from
// the Builder's built-in and user stores alike, each narrowed to its picked
// tools and always on. It returns nil when the config picks none or there are
// no MCP stores.
func (b *Builder) MCPClient(picks map[string]mcpclient.ToolFilter) agents.MCPClient {
	if len(picks) == 0 || (b.deps.BuiltInMCPStore == nil && b.deps.UserMCPStore == nil) {
		return nil
	}
	return alwaysOnConnectors{client: mcpclient.NewClient(
		mcpclient.Filter(b.deps.BuiltInMCPStore, picks),
		mcpclient.WithUserStore(mcpclient.Filter(b.deps.UserMCPStore, picks)),
		mcpclient.WithCredentials(b.deps.MCPCredentials),
		mcpclient.WithSchemaCache(b.deps.MCPSchemaCache),
	)}
}

// alwaysOnConnectors keeps every server on whatever a run selects, since each
// is one of the config's picks. A user's own server is still the user's: its
// grants and checks stay its owner's.
type alwaysOnConnectors struct{ client *mcpclient.Client }

func (c alwaysOnConnectors) ListTools(ctx context.Context, namespace string, runContext map[string]any, _ ...agents.MCPSelection) ([]agents.ConnectorStatus, []agents.Tool, error) {
	return c.client.ListTools(ctx, namespace, runContext)
}

func (c alwaysOnConnectors) CallTool(ctx context.Context, descriptor *agents.BaseTool, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
	return c.client.CallTool(ctx, descriptor, call)
}

// ListServers lists the agent's servers without connecting, every one always
// on, as a UI offering them shows them.
func (c alwaysOnConnectors) ListServers(ctx context.Context, namespace string, runContext map[string]any) ([]mcpclient.ListedServer, error) {
	listed, err := c.client.ListServers(ctx, namespace, runContext)
	for i := range listed {
		listed[i].AlwaysOn = true
	}
	return listed, err
}

var _ mcpclient.ServerLister = alwaysOnConnectors{}

// GlobalSkillNames lists the skills of the Builder's built-in store, which
// users' uploads must not reuse. It reads the store, never building an agent.
func (b *Builder) GlobalSkillNames(ctx context.Context) ([]string, error) {
	if b.deps.BuiltInSkillStore == nil {
		return nil, nil
	}
	catalog, err := skills.NewClient(b.deps.BuiltInSkillStore).ListSkills(ctx, "", nil)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(catalog))
	for _, skill := range catalog {
		names = append(names, skill.Name)
	}
	sort.Strings(names)
	return names, nil
}
