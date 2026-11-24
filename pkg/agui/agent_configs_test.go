package agui

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hastekit/agent-sdk-go/internal/testutil"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentconfig"
	"github.com/hastekit/agent-sdk-go/pkg/agents/mcpclient"
	"github.com/hastekit/agent-sdk-go/pkg/agents/skills"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type tenantRequest struct {
	server *httptest.Server
	t      *testing.T
}

func (c tenantRequest) do(tenant, method, path, body string) (int, string) {
	c.t.Helper()
	req, err := http.NewRequest(method, c.server.URL+path, strings.NewReader(body))
	require.NoError(c.t, err)
	req.Header.Set("X-Tenant", tenant)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(c.t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(c.t, err)
	return resp.StatusCode, string(data)
}

func (c tenantRequest) agentNames(tenant string) []string {
	c.t.Helper()
	status, body := c.do(tenant, "GET", "/agents", "")
	require.Equal(c.t, http.StatusOK, status, body)
	var listing struct {
		Agents       []string `json:"agents"`
		AgentConfigs bool     `json:"agent_configs"`
	}
	require.NoError(c.t, json.Unmarshal([]byte(body), &listing))
	require.True(c.t, listing.AgentConfigs)
	return listing.Agents
}

// A namespace creates an agent over HTTP at run time; it runs with only its
// picks, and no other namespace can see or run it.
func TestAgentConfigsAreCreatedAndRunPerNamespace(t *testing.T) {
	f := testutil.NewAgentFixture(t, nil, nil, testutil.FixtureSteps()...)
	static := registry{"static-helper": agents.NewAgent(&agents.AgentOptions{Name: "static-helper"})}
	reg := agentconfig.NewRegistry(f.Builder, staticAgents(static))
	userSkills, err := skills.NewFileStore(t.TempDir())
	require.NoError(t, err)
	server := httptest.NewServer(NewHandler(reg,
		WithAgentConfigStore(f.Store),
		WithSkillStore(userSkills),
		WithNamespaceResolver(func(r *http.Request) (string, error) { return r.Header.Get("X-Tenant"), nil }),
	))
	defer server.Close()
	c := tenantRequest{server: server, t: t}

	assert.Equal(t, []string{"static-helper"}, c.agentNames(testutil.FixtureNamespace))

	config, err := json.Marshal(f.Config)
	require.NoError(t, err)
	status, body := c.do(testutil.FixtureNamespace, "PUT", "/agent-configs/"+testutil.FixtureAgent, string(config))
	require.Equal(t, http.StatusOK, status, body)

	assert.Equal(t, []string{testutil.FixtureAgent, "static-helper"}, c.agentNames(testutil.FixtureNamespace))
	assert.Equal(t, []string{"static-helper"}, c.agentNames("tenant-b"), "another namespace never lists it")

	run := `{"threadId":"thread-1","messages":[{"id":"m1","role":"user","content":"go"}]}`
	status, body = c.do("tenant-b", "POST", "/agents/"+testutil.FixtureAgent+"/run", run)
	require.Equal(t, http.StatusNotFound, status, "another namespace cannot run it: %s", body)
	status, _ = c.do("tenant-b", "GET", "/agent-configs/"+testutil.FixtureAgent, "")
	require.Equal(t, http.StatusNotFound, status, "nor read its config")
	require.Empty(t, f.Provider.Seen())

	status, body = c.do(testutil.FixtureNamespace, "POST", "/agents/"+testutil.FixtureAgent+"/run", run)
	require.Equal(t, http.StatusOK, status, body)
	assert.Contains(t, body, "RUN_FINISHED")
	seen := f.Provider.Seen()
	require.Len(t, seen, 4)
	assert.Equal(t, []string{testutil.FixturePickedMCP, testutil.FixturePickedTool, agents.ReadSkillToolName}, seen[0].Tools)
	assert.Zero(t, f.Calls.Of(testutil.FixtureOtherTool))

	// The builder's global skills are reserved without building any agent.
	upload := `{"files":{"SKILL.md":"` + base64.StdEncoding.EncodeToString(
		[]byte("---\nname: "+testutil.FixtureOtherSkill+"\ndescription: mine\n---\nbody")) + `"}}`
	status, body = c.do(testutil.FixtureNamespace, "PUT", "/skills/"+testutil.FixtureOtherSkill, upload)
	require.Equal(t, http.StatusConflict, status, body)

	status, _ = c.do(testutil.FixtureNamespace, "DELETE", "/agent-configs/"+testutil.FixtureAgent, "")
	require.Equal(t, http.StatusNoContent, status)
	assert.Equal(t, []string{"static-helper"}, c.agentNames(testutil.FixtureNamespace))
}

func TestAgentConfigWritesAreValidated(t *testing.T) {
	f := testutil.NewAgentFixture(t, nil, nil)
	static := registry{"static-helper": agents.NewAgent(&agents.AgentOptions{Name: "static-helper"})}
	builtIns, err := agentconfig.NewInlineStore(agentconfig.Config{Name: "concierge", Model: testutil.FixtureModel})
	require.NoError(t, err)
	builder, err := agentconfig.NewBuilder(agentconfig.Dependencies{
		Stores: []agentconfig.Store{builtIns, f.Store},
		Models: agentconfig.Models{testutil.FixtureModel: f.Provider},
	})
	require.NoError(t, err)
	reg := agentconfig.NewRegistry(builder, staticAgents(static))
	server := httptest.NewServer(NewHandler(reg, WithAgentConfigStore(f.Store),
		WithNamespaceResolver(func(r *http.Request) (string, error) { return r.Header.Get("X-Tenant"), nil })))
	defer server.Close()
	c := tenantRequest{server: server, t: t}

	for _, tc := range []struct {
		name, path, body string
		want             int
	}{
		{"a code agent's name", "/agent-configs/static-helper", `{"model":"` + testutil.FixtureModel + `"}`, http.StatusConflict},
		{"a built-in's name", "/agent-configs/concierge", `{"model":"` + testutil.FixtureModel + `"}`, http.StatusConflict},
		{"a model off the allowlist", "/agent-configs/mine", `{"model":"OpenAI/gpt-4o"}`, http.StatusBadRequest},
		{"a tool outside the catalog", "/agent-configs/mine", `{"model":"` + testutil.FixtureModel + `","tools":["rm_rf"]}`, http.StatusBadRequest},
		{"a name that differs from the route", "/agent-configs/mine", `{"name":"other","model":"` + testutil.FixtureModel + `"}`, http.StatusBadRequest},
		{"unknown fields", "/agent-configs/mine", `{"model":"` + testutil.FixtureModel + `","runtime":"x"}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := c.do("tenant-a", "PUT", tc.path, tc.body)
			require.Equal(t, tc.want, status, body)
		})
	}

	// Built-ins run in every namespace, and none can overwrite them.
	assert.Contains(t, c.agentNames("tenant-a"), "concierge")
	assert.Contains(t, c.agentNames("tenant-b"), "concierge")
}

// The management handler refuses the built-in namespace whatever its resolver
// says, so no request writes it.
func TestAgentConfigHandlerNeverWritesTheBuiltInNamespace(t *testing.T) {
	store := agentconfig.NewMemoryStore()
	for _, resolve := range []func(*http.Request) (string, error){
		nil,
		func(*http.Request) (string, error) { return "", nil },
		func(*http.Request) (string, error) { return "  ", nil },
	} {
		var options []agentconfig.HandlerOption
		if resolve != nil {
			options = append(options, agentconfig.WithNamespaceResolver(resolve))
		}
		h := agentconfig.NewHandler(store, options...)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("PUT", "/agent-configs/x", strings.NewReader(`{"model":"m"}`)))
		require.Equal(t, http.StatusForbidden, w.Code)
	}
	listing, err := agentconfig.NewCatalog(store).List(t.Context(), "")
	require.NoError(t, err)
	require.Empty(t, listing.BuiltIn)
}

// staticAgents adapts the test registry, which already speaks Registry, to
// the code-built StaticAgents shape.
type staticAgents registry

func (s staticAgents) Agent(name string) (*agents.Agent, bool) {
	a, ok := s[name]
	return a, ok
}

func (s staticAgents) AgentNames() []string {
	names := make([]string, 0, len(s))
	for name := range s {
		names = append(names, name)
	}
	return names
}

func TestStaticRegistryOffersTheSameAgentsToEveryNamespace(t *testing.T) {
	helper := agents.NewAgent(&agents.AgentOptions{Name: "helper"})
	reg := StaticRegistry(staticAgents{"helper": helper})
	for _, namespace := range []string{"a", "b"} {
		agent, ok, err := reg.Agent(t.Context(), namespace, "helper")
		require.NoError(t, err)
		require.True(t, ok)
		require.Same(t, helper, agent)
		names, err := reg.AgentNames(t.Context(), namespace)
		require.NoError(t, err)
		require.Equal(t, []string{"helper"}, names)
	}
	_, ok, err := reg.Agent(t.Context(), "a", "missing")
	require.NoError(t, err)
	require.False(t, ok)
}

// The embedded editor offers what a config may pick: nothing is typed blind.
func TestAgentChoicesListWhatAConfigMayPick(t *testing.T) {
	f := testutil.NewAgentFixture(t, nil, nil)
	f.AddConfig(t)
	reg := agentconfig.NewRegistry(f.Builder, staticAgents(registry{"static-helper": agents.NewAgent(&agents.AgentOptions{Name: "static-helper"})}))
	server := httptest.NewServer(NewHandler(reg, WithAgentConfigStore(f.Store),
		WithNamespaceResolver(func(r *http.Request) (string, error) { return r.Header.Get("X-Tenant"), nil })))
	defer server.Close()
	c := tenantRequest{server: server, t: t}

	status, body := c.do(testutil.FixtureNamespace, "GET", "/agent-choices", "")
	require.Equal(t, http.StatusOK, status, body)
	var choices agentconfig.Choices
	require.NoError(t, json.Unmarshal([]byte(body), &choices))
	names := func(items []agentconfig.Choice) []string {
		out := []string{}
		for _, item := range items {
			out = append(out, item.Name)
		}
		return out
	}
	assert.Equal(t, []string{testutil.FixtureModel}, choices.Models)
	assert.Equal(t, []string{testutil.FixtureOtherTool, testutil.FixtureBackgroundTool, testutil.FixturePickedTool}, names(choices.Tools))
	assert.Equal(t, []string{testutil.FixturePickedSkill, testutil.FixtureOtherSkill}, names(choices.Skills))
	assert.Equal(t, []string{testutil.FixtureOtherServer, "docs"}, names(choices.Connectors))
	assert.Equal(t, []string{testutil.FixtureAgent}, names(choices.Agents), "a code agent is no handoff choice")

	status, body = c.do("tenant-b", "GET", "/agent-choices", "")
	require.Equal(t, http.StatusOK, status, body)
	require.NoError(t, json.Unmarshal([]byte(body), &choices))
	assert.Empty(t, names(choices.Agents), "another namespace's agents are not handoff choices")

	// Nor can a config name one: the edge could never run.
	status, body = c.do(testutil.FixtureNamespace, "PUT", "/agent-configs/mine",
		`{"model":"`+testutil.FixtureModel+`","handoffs":[{"name":"static-helper"}]}`)
	require.Equal(t, http.StatusBadRequest, status, body)
	assert.Contains(t, body, "built in code")
	status, body = c.do(testutil.FixtureNamespace, "PUT", "/agent-configs/mine",
		`{"model":"`+testutil.FixtureModel+`","sub_agents":[{"name":"static-helper"}]}`)
	require.Equal(t, http.StatusBadRequest, status, body, "nor call one as a sub-agent")
	assert.Contains(t, body, "built in code")
	status, body = c.do(testutil.FixtureNamespace, "PUT", "/agent-configs/mine",
		`{"model":"`+testutil.FixtureModel+`","sub_agents":[{"name":"`+testutil.FixtureAgent+`"}]}`)
	require.Equal(t, http.StatusOK, status, body, "another config is a sub-agent like any")
}

// Agents users create must be agents the registry runs, so the handler is
// refused a store its registry does not read.
func TestAgentConfigStoreNeedsARegistryThatRunsIt(t *testing.T) {
	f := testutil.NewAgentFixture(t, nil, nil)
	static := staticAgents{"static-helper": agents.NewAgent(&agents.AgentOptions{Name: "static-helper"})}
	assert.PanicsWithValue(t, agentConfigRegistryPanic, func() {
		NewHandler(StaticRegistry(static), WithAgentConfigStore(f.Store))
	}, "code agents alone run no configs")
	assert.PanicsWithValue(t, agentConfigRegistryPanic, func() {
		NewHandler(agentconfig.NewRegistry(f.Builder, static), WithAgentConfigStore(agentconfig.NewMemoryStore()))
	}, "the builder never reads another store")
	assert.NotPanics(t, func() {
		NewHandler(agentconfig.NewRegistry(f.Builder, static), WithAgentConfigStore(f.Store))
	})
}

// The embedded UI asks each agent which MCP servers it has: a config agent its
// picks, always on; a code agent the servers of its own client; an agent
// without a client none.
func TestAgentRoutesNameTheAgentsOwnServers(t *testing.T) {
	f := testutil.NewAgentFixture(t, nil, nil)
	f.AddConfig(t)
	withClient := agents.NewAgent(&agents.AgentOptions{Name: "with-client", MCPClient: mcpclient.NewClient(
		mcpclient.NewInlineStore(mcpclient.ServerConfig{Name: "global-docs", Endpoint: "https://docs.example/mcp"}))})
	static := registry{"without-client": agents.NewAgent(&agents.AgentOptions{Name: "without-client"}), "with-client": withClient}
	server := httptest.NewServer(NewHandler(agentconfig.NewRegistry(f.Builder, staticAgents(static)), WithAgentConfigStore(f.Store),
		WithNamespaceResolver(func(r *http.Request) (string, error) { return r.Header.Get("X-Tenant"), nil })))
	defer server.Close()
	c := tenantRequest{server: server, t: t}

	for agent, want := range map[string]string{
		testutil.FixtureAgent: `{"servers":[{"name":"docs","always_on":true}]}`,
		"with-client":         `{"servers":[{"name":"global-docs","always_on":true}]}`,
		"without-client":      `{"servers":[]}`,
	} {
		status, body := c.do(testutil.FixtureNamespace, "GET", "/agents/"+agent+"/mcp", "")
		require.Equal(t, http.StatusOK, status, body)
		assert.JSONEq(t, want, body, agent)
	}
	status, _ := c.do("tenant-b", "GET", "/agents/"+testutil.FixtureAgent+"/mcp", "")
	require.Equal(t, http.StatusNotFound, status)

	status, body := c.do(testutil.FixtureNamespace, "GET", "/agents/"+testutil.FixtureAgent+"/skills", "")
	require.Equal(t, http.StatusOK, status, body)
	assert.Contains(t, body, testutil.FixturePickedSkill)
	assert.NotContains(t, body, testutil.FixtureOtherSkill, "a global the config did not pick is not the agent's")
}
