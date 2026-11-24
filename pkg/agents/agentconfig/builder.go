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
// required.
type Dependencies struct {
	// Stores are where configs are read, with built-ins in their empty
	// namespace. The same stores must be readable wherever agents are looked up
	// by name; durable workers do not read them, since a run carries its
	// snapshot.
	Stores []Store

	// Models allowlists the model ids configs may name.
	Models ModelResolver

	// SkillStores are the unfiltered skill stores; each agent sees only its
	// picks, through skills.Filter.
	SkillStores []skills.Store

	// MCPStores are the unfiltered MCP stores; each agent sees only its picks,
	// through mcpclient.Filter. MCPCredentials and MCPSchemaCache are shared by
	// every agent's client.
	MCPStores      []mcpclient.MCPServerConfigStore
	MCPCredentials mcpclient.CredentialProvider
	MCPSchemaCache mcpclient.SchemaCache

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

	// CacheSize bounds how many built agents are kept for reuse, keyed by
	// their snapshot, so a request does not rebuild an unchanged agent. Zero
	// uses 256; a negative size disables the cache.
	CacheSize int
}

// Builder turns configs into plain agents.Agent values from code-side
// Dependencies. It is safe for concurrent use.
//
// Durable workers use the same Builder to rebuild each dependency from the
// part of a config an activity carries — see Model, Tool, Prompt, SkillClient
// and MCPClient — so the worker and the caller must be given equal
// Dependencies.
type Builder struct {
	deps    Dependencies
	catalog *Catalog
	broker  agents.StreamBroker
	history *history.CommonConversationManager
	cache   *agentCache
}

// NewBuilder validates the dependencies and copies the slices and maps it is
// given, so later changes to them do not apply.
func NewBuilder(deps Dependencies) (*Builder, error) {
	if deps.Models == nil {
		return nil, errors.New("agentconfig: a ModelResolver is required")
	}
	deps.Stores = slices.Clone(deps.Stores)
	deps.SkillStores = slices.Clone(deps.SkillStores)
	deps.MCPStores = slices.Clone(deps.MCPStores)
	deps.Middlewares = slices.Clone(deps.Middlewares)
	deps.PromptOptions = slices.Clone(deps.PromptOptions)
	deps.Tools = maps.Clone(deps.Tools)
	for name, tool := range deps.Tools {
		if tool == nil || tool.GetToolDescriptor() == nil {
			return nil, fmt.Errorf("agentconfig: catalog tool %q has no descriptor", name)
		}
	}
	for _, store := range deps.SkillStores {
		if store == nil {
			return nil, errors.New("agentconfig: skill store must not be nil")
		}
	}
	for _, store := range deps.MCPStores {
		if store == nil {
			return nil, errors.New("agentconfig: MCP store must not be nil")
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

	size := deps.CacheSize
	if size == 0 {
		size = 256
	}
	return &Builder{
		deps:    deps,
		catalog: NewCatalog(deps.Stores...),
		broker:  broker,
		history: historyManager,
		cache:   newAgentCache(size),
	}, nil
}

// Catalog reads the Builder's stores.
func (b *Builder) Catalog() *Catalog { return b.catalog }

// StreamBroker is the broker built agents stream on.
func (b *Builder) StreamBroker() agents.StreamBroker { return b.broker }

// History persists every built agent's conversations.
func (b *Builder) History() *history.CommonConversationManager { return b.history }

// Middlewares wrap every built agent's calls.
func (b *Builder) Middlewares() []agents.Middleware { return slices.Clone(b.deps.Middlewares) }

// ClientTools configures client-run tools for every built agent.
func (b *Builder) ClientTools() agents.ClientToolOptions { return b.deps.ClientTools }

// Agent resolves name for namespace and builds it, reusing a cached build when
// nothing in its graph changed. It returns ErrNotFound for a name the
// namespace cannot run.
func (b *Builder) Agent(ctx context.Context, namespace, name string) (*agents.Agent, error) {
	snapshot, err := b.catalog.Snapshot(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	return b.Build(snapshot)
}

// Snapshot freezes the graph a run in namespace entering at name would use.
func (b *Builder) Snapshot(ctx context.Context, namespace, name string) (*Snapshot, error) {
	return b.catalog.Snapshot(ctx, namespace, name)
}

// Build turns a snapshot into a plain agent: agents.NewAgent for every config
// in the graph, handoffs wired between them, the root bound to the durable
// Runtime when there is one.
func (b *Builder) Build(snapshot *Snapshot) (*agents.Agent, error) {
	if err := snapshot.Check(snapshot.Namespace); err != nil {
		return nil, err
	}
	key, err := snapshot.fingerprint()
	if err != nil {
		return nil, err
	}
	if agent, ok := b.cache.get(key); ok {
		return agent, nil
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
	root := built[snapshot.Root]
	b.cache.put(key, root)
	return root, nil
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

// SkillClient reads a config's skills from the Builder's skill stores: the
// named skills, which are the agent's own and always on, as a developer's
// global skills are for an agent built in code; and the user's other skills,
// which a run may turn off (agents.SkillSelection). The developer's globals
// the config does not name are not the agent's. It returns nil when there are
// no skill stores.
func (b *Builder) SkillClient(names []string) agents.SkillClient {
	if len(b.deps.SkillStores) == 0 {
		return nil
	}
	stores := make([]skills.Store, 0, len(b.deps.SkillStores))
	for _, store := range b.deps.SkillStores {
		stores = append(stores, skills.Filter(store, names, skills.KeepUserSkills()))
	}
	pinned := make(map[string]bool, len(names))
	for _, name := range names {
		pinned[name] = true
	}
	return &pickedSkills{client: skills.NewClient(stores...), picked: pinned}
}

// pickedSkills lists a config's picks as global — the agent's own, which no
// run turns off — and the user's other skills as they are.
type pickedSkills struct {
	client *skills.Client
	picked map[string]bool
}

func (s *pickedSkills) ListSkills(ctx context.Context, namespace string, runContext map[string]any) ([]agents.Skill, error) {
	listed, err := s.client.ListSkills(ctx, namespace, runContext)
	if err != nil {
		return nil, err
	}
	for i := range listed {
		if s.picked[listed[i].Name] {
			listed[i].Global = true
		}
	}
	return listed, nil
}

func (s *pickedSkills) ReadSkill(ctx context.Context, namespace string, runContext map[string]any, name, file string) (string, error) {
	return s.client.ReadSkill(ctx, namespace, runContext, name, file)
}

// MCPClient reaches a config's connectors through the Builder's MCP stores:
// the picked ones, each narrowed to its picked tools, which are the agent's own
// and always on, as a developer's global servers are for an agent built in
// code; and the user's other servers, which a run may turn off or narrow
// (agents.MCPSelection). The developer's globals the config does not pick are
// not the agent's. It returns nil when there are no MCP stores.
func (b *Builder) MCPClient(picks map[string]mcpclient.ToolFilter) agents.MCPClient {
	if len(b.deps.MCPStores) == 0 {
		return nil
	}
	stores := make([]mcpclient.MCPServerConfigStore, 0, len(b.deps.MCPStores))
	for _, store := range b.deps.MCPStores {
		stores = append(stores, mcpclient.Filter(store, picks, mcpclient.KeepUserServers()))
	}
	client := mcpclient.NewClient(stores...)
	if b.deps.MCPCredentials != nil {
		client = client.WithCredentials(b.deps.MCPCredentials)
	}
	if b.deps.MCPSchemaCache != nil {
		client = client.WithSchemaCache(b.deps.MCPSchemaCache)
	}
	return &pickedConnectors{client: client, picked: picks}
}

// pickedConnectors keeps a config's picked servers on whatever a run selects:
// a user's own server is still the user's — its grants and checks stay its
// owner's — but a run may only turn off or narrow the servers it did not pick.
type pickedConnectors struct {
	client *mcpclient.Client
	picked map[string]mcpclient.ToolFilter
}

func (c *pickedConnectors) ListTools(ctx context.Context, namespace string, runContext map[string]any, selection ...agents.MCPSelection) ([]agents.ConnectorStatus, []agents.Tool, error) {
	kept := make([]agents.MCPSelection, 0, len(selection))
	for _, s := range selection {
		next := agents.MCPSelection{}
		for _, name := range s.Disable {
			if _, picked := c.picked[name]; !picked {
				next.Disable = append(next.Disable, name)
			}
		}
		for name, tools := range s.Tools {
			if _, picked := c.picked[name]; !picked {
				if next.Tools == nil {
					next.Tools = map[string]agents.MCPToolSelection{}
				}
				next.Tools[name] = tools
			}
		}
		kept = append(kept, next)
	}
	return c.client.ListTools(ctx, namespace, runContext, kept...)
}

func (c *pickedConnectors) CallTool(ctx context.Context, descriptor *agents.BaseTool, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
	return c.client.CallTool(ctx, descriptor, call)
}

// ListServers lists the agent's servers without connecting, its picks always
// on, as a UI offering them shows them.
func (c *pickedConnectors) ListServers(ctx context.Context, namespace string, runContext map[string]any) ([]mcpclient.ListedServer, error) {
	listed, err := c.client.ListServers(ctx, namespace, runContext)
	if err != nil {
		return nil, err
	}
	for i := range listed {
		if _, picked := c.picked[listed[i].Name]; picked {
			listed[i].AlwaysOn = true
		}
	}
	return listed, nil
}

var _ mcpclient.ServerLister = (*pickedConnectors)(nil)

// GlobalSkillNames lists the global skills of the Builder's unfiltered skill
// stores: every skill any config could pick, which users' uploads must not
// reuse. It reads the stores, never building an agent.
func (b *Builder) GlobalSkillNames(ctx context.Context) ([]string, error) {
	if len(b.deps.SkillStores) == 0 {
		return nil, nil
	}
	catalog, err := skills.NewClient(b.deps.SkillStores...).ListSkills(ctx, "", nil)
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

// agentCache is a small LRU of built agents by snapshot fingerprint: a map
// for lookup and a doubly linked list, most recently used first, for eviction.
type agentCache struct {
	mu      sync.Mutex
	size    int
	entries map[string]*cacheEntry
	newest  *cacheEntry
	oldest  *cacheEntry
}

type cacheEntry struct {
	key          string
	agent        *agents.Agent
	newer, older *cacheEntry
}

func newAgentCache(size int) *agentCache {
	return &agentCache{size: size, entries: map[string]*cacheEntry{}}
}

func (c *agentCache) get(key string) (*agents.Agent, bool) {
	if c.size <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	c.unlink(entry)
	c.pushNewest(entry)
	return entry.agent, true
}

func (c *agentCache) put(key string, agent *agents.Agent) {
	if c.size <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.entries[key]; ok {
		entry.agent = agent
		c.unlink(entry)
		c.pushNewest(entry)
		return
	}
	entry := &cacheEntry{key: key, agent: agent}
	c.entries[key] = entry
	c.pushNewest(entry)
	for len(c.entries) > c.size {
		evicted := c.oldest
		c.unlink(evicted)
		delete(c.entries, evicted.key)
	}
}

func (c *agentCache) pushNewest(entry *cacheEntry) {
	entry.older = c.newest
	entry.newer = nil
	if c.newest != nil {
		c.newest.newer = entry
	}
	c.newest = entry
	if c.oldest == nil {
		c.oldest = entry
	}
}

func (c *agentCache) unlink(entry *cacheEntry) {
	if entry.newer != nil {
		entry.newer.older = entry.older
	} else {
		c.newest = entry.older
	}
	if entry.older != nil {
		entry.older.newer = entry.newer
	} else {
		c.oldest = entry.newer
	}
	entry.newer, entry.older = nil, nil
}
