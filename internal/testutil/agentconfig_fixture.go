package testutil

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentconfig"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/mcpclient"
	"github.com/hastekit/agent-sdk-go/pkg/agents/skills"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// Names in the dynamic-agent fixture. The config picks one of each pair; the
// other must never reach the model.
const (
	FixtureModel        = "Test/scripted"
	FixtureNamespace    = "tenant-a"
	FixtureAgent        = "researcher"
	FixturePickedTool   = "lookup"
	FixtureOtherTool    = "delete_everything"
	FixturePickedSkill  = "release-notes"
	FixtureOtherSkill   = "secret-playbook"
	FixturePickedMCP    = "docs__search"
	FixtureOtherMCPTool = "docs__admin"
	FixtureOtherServer  = "billing"
	// FixtureBackgroundTool starts a background task, "task-1", whose result
	// is FixtureBackgroundResult. No config picks it unless a test does.
	FixtureBackgroundTool   = "index_docs"
	FixtureBackgroundResult = "indexed 42 documents"
)

// Calls counts what ran, by tool name.
type Calls struct {
	mu    sync.Mutex
	count map[string]int
}

func (c *Calls) add(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.count == nil {
		c.count = map[string]int{}
	}
	c.count[name]++
}

// Of returns how often name ran.
func (c *Calls) Of(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count[name]
}

// AgentFixture is a Builder over a small catalog — two tools, two global
// skills, two MCP servers — and a Registry over a store holding one config, in
// FixtureNamespace, that picks one tool, one skill and one MCP tool.
type AgentFixture struct {
	Provider *ScriptedProvider
	Store    *agentconfig.MemoryStore
	Builder  *agentconfig.Builder
	Registry *agentconfig.Registry
	Calls    *Calls
	Config   agentconfig.Config
}

// Agent resolves name for namespace through the fixture's Registry and builds
// it, returning agentconfig.ErrNotFound for a name the namespace cannot run.
func (f *AgentFixture) Agent(ctx context.Context, namespace, name string) (*agents.Agent, error) {
	agent, ok, err := f.Registry.Agent(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, agentconfig.ErrNotFound
	}
	return agent, nil
}

// FixtureSteps is a script that uses every pick once, then answers.
func FixtureSteps() []Step {
	return []Step{
		{Tool: FixturePickedTool},
		{Tool: FixturePickedMCP},
		{Tool: agents.ReadSkillToolName, Arguments: `{"name":"` + FixturePickedSkill + `"}`},
		{Text: "all done"},
	}
}

// NewAgentFixture builds the fixture. runtime may be nil for local runs;
// broker, when set, is used for local runs; history may be nil.
func NewAgentFixture(t *testing.T, runtime agentconfig.Runtime, broker agents.StreamBroker, steps ...Step) *AgentFixture {
	t.Helper()
	calls := &Calls{}
	provider := NewScriptedProvider(steps...)

	skillStore, err := skills.NewInlineStore(
		skillBundle(FixturePickedSkill, "How to write release notes."),
		skillBundle(FixtureOtherSkill, "Never shown to this agent."),
	)
	require.NoError(t, err)

	docs := mcpServer(t, calls, "docs", "search", "admin")
	billing := mcpServer(t, calls, FixtureOtherServer, "charge")
	mcpStore := mcpclient.NewInlineStore(
		mcpclient.ServerConfig{Name: "docs", Endpoint: docs, Transport: mcpclient.TransportStreamableHTTP, ToolPrefix: "docs__"},
		mcpclient.ServerConfig{Name: FixtureOtherServer, Endpoint: billing, Transport: mcpclient.TransportStreamableHTTP, ToolPrefix: FixtureOtherServer + "__"},
	)

	store := agentconfig.NewMemoryStore()
	cfg := agentconfig.Config{
		Name:        FixtureAgent,
		Description: "Looks things up.",
		Model:       FixtureModel,
		Instruction: "You research questions.",
		MaxLoops:    8,
		Skills:      []string{FixturePickedSkill},
		Tools:       []string{FixturePickedTool},
		MCP:         map[string]mcpclient.ToolFilter{"docs": {Include: []string{"search"}}},
	}
	builder, err := agentconfig.NewBuilder(agentconfig.Dependencies{
		Models:            agentconfig.Models{FixtureModel: provider},
		BuiltInSkillStore: skillStore,
		BuiltInMCPStore:   mcpStore,
		Tools: map[string]agents.Tool{
			FixturePickedTool:     newRecordingTool(calls, FixturePickedTool),
			FixtureOtherTool:      newRecordingTool(calls, FixtureOtherTool),
			FixtureBackgroundTool: &backgroundTool{recordingTool: newRecordingTool(calls, FixtureBackgroundTool)},
		},
		History:      history.NewConversationManager(history.NewInMemoryConversationPersistence()),
		StreamBroker: broker,
		Runtime:      runtime,
	})
	require.NoError(t, err)
	registry, err := agentconfig.NewRegistry(builder, nil, store)
	require.NoError(t, err)
	return &AgentFixture{Provider: provider, Store: store, Builder: builder, Registry: registry, Calls: calls, Config: cfg}
}

// AddConfig writes the fixture's config to its namespace, as a user would
// after the worker started.
func (f *AgentFixture) AddConfig(t *testing.T) {
	t.Helper()
	_, err := f.Store.Put(context.Background(), FixtureNamespace, f.Config)
	require.NoError(t, err)
}

func skillBundle(name, description string) skills.Bundle {
	return skills.Bundle{Files: map[string][]byte{
		"SKILL.md": []byte("---\nname: " + name + "\ndescription: " + description + "\n---\n\nInstructions for " + name + "."),
	}}
}

func mcpServer(t *testing.T, calls *Calls, label string, tools ...string) string {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: label, Version: "1"}, nil)
	for _, name := range tools {
		toolName := label + "__" + name
		mcp.AddTool(server, &mcp.Tool{Name: name}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			calls.add(toolName)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: toolName + " result"}}}, nil, nil
		})
	}
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{DisableLocalhostProtection: true}))
	t.Cleanup(func() { httpServer.CloseClientConnections(); httpServer.Close() })
	return httpServer.URL
}

type recordingTool struct {
	*agents.BaseTool
	calls *Calls
	name  string
}

func newRecordingTool(calls *Calls, name string) *recordingTool {
	return &recordingTool{calls: calls, name: name, BaseTool: &agents.BaseTool{ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{
		Name:        name,
		Description: utils.Ptr("A catalog tool."),
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
	}}}}
}

func (t *recordingTool) Execute(_ context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	t.calls.add(t.name)
	return &agents.ToolCallResponse{FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
		ID: params.ID, CallID: params.CallID,
		Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr(t.name + " result")},
	}}, nil
}

// backgroundTool answers with a task id, then reports the task done when
// waited on.
type backgroundTool struct{ *recordingTool }

func (t *backgroundTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	resp, err := t.recordingTool.Execute(ctx, params)
	if err != nil {
		return nil, err
	}
	resp.TaskID = "task-1"
	return resp, nil
}

func (t *backgroundTool) AwaitTask(ctx context.Context, _ agents.BackgroundTaskRef, progress agents.ProgressReporter) (agents.BackgroundResult, error) {
	t.calls.add(FixtureBackgroundTool + ":await")
	return agents.BackgroundResult{Output: agents.BackgroundText(FixtureBackgroundResult)}, nil
}
