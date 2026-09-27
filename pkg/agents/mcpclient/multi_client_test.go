package mcpclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// catalogServer supplies a real MCP transport so selection and execution are tested together.
func catalogServer(t *testing.T, label string) string {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: label, Version: "1"}, nil)
	for _, name := range []string{"read", "write", "hidden"} {
		mcp.AddTool(server, &mcp.Tool{Name: name}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: label}}}, nil, nil
		})
	}
	// User fixtures present a public Host while their test socket dialer routes to loopback.
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{DisableLocalhostProtection: true}))
	t.Cleanup(func() { httpServer.CloseClientConnections(); httpServer.Close() })
	return httpServer.URL
}

func catalogConfig(name, endpoint string) ServerConfig {
	return ServerConfig{Name: name, Endpoint: endpoint, Transport: TransportStreamableHTTP, ToolPrefix: name + "__"}
}

func TestConfigStoresScopeIsolationAndRoundTrip(t *testing.T) {
	type writableStore interface {
		MCPServerConfigStore
		Put(context.Context, string, ServerConfig) error
		Delete(context.Context, string, string) error
	}
	for _, kind := range []string{"memory", "file"} {
		t.Run(kind, func(t *testing.T) {
			// Both implementations keep one name per scope and return global plus only the requested user.
			var store writableStore = NewMemoryStore()
			path := filepath.Join(t.TempDir(), "servers.json")
			if kind == "file" {
				var err error
				store, err = NewFileStore(path)
				require.NoError(t, err)
			}
			for _, scope := range []string{"", "alice", "bob"} {
				config := catalogConfig("same", "https://example.test/"+scope)
				config.Namespace = "forged"
				require.NoError(t, store.Put(t.Context(), scope, config))
			}
			visible, err := store.ListServerConfigs(t.Context(), "alice", nil)
			require.NoError(t, err)
			require.Len(t, visible, 2)
			require.Equal(t, "", visible[0].Namespace)
			require.Equal(t, "alice", visible[1].Namespace)

			// Namespace writes and deletes never modify a developer definition with the same name.
			require.NoError(t, store.Delete(t.Context(), "alice", "same"))
			visible, err = store.ListServerConfigs(t.Context(), "alice", nil)
			require.NoError(t, err)
			require.Len(t, visible, 1)
			visible, err = store.ListServerConfigs(t.Context(), "bob", nil)
			require.NoError(t, err)
			require.Len(t, visible, 2)
			if kind == "file" {
				reopened, err := NewFileStore(path)
				require.NoError(t, err)
				again, err := reopened.ListServerConfigs(t.Context(), "bob", nil)
				require.NoError(t, err)
				require.Equal(t, visible, again)
			}
		})
	}
}

func TestClientGlobalPrecedenceSelectionAndExecution(t *testing.T) {
	store := NewMemoryStore()
	global := catalogConfig("docs", catalogServer(t, "global"))
	global.ToolFilter = ToolFilter{Include: []string{"read", "write"}}
	require.NoError(t, store.Put(t.Context(), "", global))
	require.NoError(t, store.Put(t.Context(), "alice", catalogConfig("docs", "https://down.invalid/mcp")))
	user := catalogConfig("private", userCatalogServer(t, "alice"))
	require.NoError(t, store.Put(t.Context(), "alice", user))
	require.NoError(t, store.Put(t.Context(), "bob", catalogConfig("bob", "https://down.invalid/mcp")))
	client := NewClient(store)

	// User configuration cannot replace a global destination or re-enable its excluded tools.
	input := &agents.AgentInput{Namespace: "alice", MCP: agents.MCPSelection{
		Tools: map[string]agents.MCPToolSelection{"docs": {Include: []string{"read", "hidden"}}, "private": {Exclude: []string{"write", "hidden"}}},
	}}
	statuses, tools, err := client.ListTools(t.Context(), input.Namespace, input.RunContext, input.MCP)
	require.NoError(t, err)
	require.Len(t, statuses, 2)
	require.Len(t, tools, 2)
	require.Equal(t, 1, statuses[0].ToolCount)
	require.Equal(t, "docs__read", tools[0].GetToolDescriptor().ToolUnion.OfFunction.Name)
	call := &agents.ToolCall{Namespace: "alice", FunctionCallMessage: &responses.FunctionCallMessage{Name: "docs__read", Arguments: "{}"}}
	output, err := tools[0].Execute(t.Context(), call)
	require.NoError(t, err)
	require.Contains(t, *output.Output.OfString, "global")

	// Global tools resolve for the execution namespace, including through durable execution.
	call.Namespace = "bob"
	output, err = client.CallTool(t.Context(), tools[0].GetToolDescriptor(), call)
	require.NoError(t, err)
	require.Contains(t, *output.Output.OfString, "global")
	statuses, tools, err = client.ListTools(t.Context(), "alice", nil, agents.MCPSelection{Enable: []string{"private"}, Disable: []string{"docs", "private"}})
	require.NoError(t, err)
	require.Len(t, tools, 2, "global tools remain enabled despite the disable selection")
	require.Equal(t, []agents.ConnectorStatus{agents.ConnectedConnectorStatus("docs", 2)}, statuses)
}

func TestCallToolRoutesByServerNameAndExecutionNamespace(t *testing.T) {
	// Each namespace owns a distinct destination under the same connector name.
	store := NewMemoryStore()
	for _, namespace := range []string{"alice", "bob"} {
		require.NoError(t, store.Put(t.Context(), namespace, catalogConfig("docs", userCatalogServer(t, namespace))))
	}
	client := NewClient(store)
	_, tools, err := client.ListTools(t.Context(), "alice", nil)
	require.NoError(t, err)
	require.NotEmpty(t, tools)
	descriptor := tools[0].GetToolDescriptor()
	require.Equal(t, "docs", descriptor.MCPServerName)
	require.Equal(t, "docs", descriptor.Meta["server_name"])

	// Routing uses the explicit server field and call namespace, never metadata or run context.
	descriptor.Meta["server_name"] = "metadata-only"
	call := &agents.ToolCall{
		Namespace: "bob", RunContext: map[string]any{"namespace": "alice"},
		FunctionCallMessage: &responses.FunctionCallMessage{Name: "docs__read", Arguments: "{}"},
	}
	result, err := tools[0].Execute(t.Context(), call)
	require.NoError(t, err)
	require.Contains(t, *result.Output.OfString, "bob")
	require.Equal(t, "metadata-only", descriptor.Meta["server_name"])

	// Metadata alone cannot substitute for the serialized server routing field.
	descriptor.MCPServerName = ""
	_, err = client.CallTool(t.Context(), descriptor, call)
	require.ErrorContains(t, err, "server name")
}

func TestNewNamespaceServersAreEnabledWithExistingSelection(t *testing.T) {
	// An explicit disable skips discovery and credential setup for that connector.
	store := NewMemoryStore()
	// OAuth without a provider would fail this connector if it were ever configured.
	disabled := catalogConfig("disabled", "https://down.invalid/mcp")
	disabled.Authorization = &OAuthConfig{ClientID: "client", RedirectURL: "https://app.example/cb", AuthURL: "https://accounts.example/authorize", TokenURL: "https://accounts.example/token"}
	require.NoError(t, store.Put(t.Context(), "alice", disabled))
	client := NewClient(store)
	selection := agents.MCPSelection{Disable: []string{"disabled"}}
	statuses, tools, err := client.ListTools(t.Context(), "alice", nil, selection)
	require.NoError(t, err)
	require.Empty(t, tools)
	require.Empty(t, statuses, "disabled connectors are omitted from discovery results")

	// Adding a server makes it available with the same selection and client instance.
	require.NoError(t, store.Put(t.Context(), "alice", catalogConfig("new", userCatalogServer(t, "new"))))
	statuses, tools, err = client.ListTools(t.Context(), "alice", nil, selection)
	require.NoError(t, err)
	require.Len(t, tools, 3)
	require.Equal(t, []agents.ConnectorStatus{agents.ConnectedConnectorStatus("new", 3)}, statuses)

	// Disabling removes both tools and status; clearing that choice restores both.
	selection.Disable = append(selection.Disable, "new")
	statuses, tools, err = client.ListTools(t.Context(), "alice", nil, selection)
	require.NoError(t, err)
	require.Empty(t, tools)
	require.Empty(t, statuses)
	selection.Disable = []string{"disabled"}
	statuses, tools, err = client.ListTools(t.Context(), "alice", nil, selection)
	require.NoError(t, err)
	require.Len(t, tools, 3)
	require.Equal(t, []agents.ConnectorStatus{agents.ConnectedConnectorStatus("new", 3)}, statuses)
}

func TestClientUsesCurrentStoreConfiguration(t *testing.T) {
	store := NewMemoryStore()
	client := NewClient(store)
	input := &agents.AgentInput{Namespace: "alice"}
	_, tools, err := client.ListTools(t.Context(), input.Namespace, input.RunContext, input.MCP)
	require.NoError(t, err)
	require.Empty(t, tools)

	// A server added after client construction is available without rebuilding the agent or worker.
	config := catalogConfig("dynamic", userCatalogServer(t, "first"))
	require.NoError(t, store.Put(t.Context(), "alice", config))
	_, tools, err = client.ListTools(t.Context(), input.Namespace, input.RunContext, input.MCP)
	require.NoError(t, err)
	require.NotEmpty(t, tools)
	old := tools[0]
	config.Endpoint = userCatalogServer(t, "second")
	require.NoError(t, store.Put(t.Context(), "alice", config))
	call := &agents.ToolCall{Namespace: "alice", FunctionCallMessage: &responses.FunctionCallMessage{Name: "dynamic__read", Arguments: "{}"}}
	updated, err := old.Execute(t.Context(), call)
	require.NoError(t, err)
	require.Contains(t, *updated.Output.OfString, "second")

	// New discovery uses the replacement definition, while deletion makes old references unusable.
	_, tools, err = client.ListTools(t.Context(), input.Namespace, input.RunContext, input.MCP)
	require.NoError(t, err)
	result, err := tools[0].Execute(t.Context(), call)
	require.NoError(t, err)
	require.Contains(t, *result.Output.OfString, "second")
	require.NoError(t, store.Delete(t.Context(), "alice", "dynamic"))
	_, err = tools[0].Execute(t.Context(), call)
	require.ErrorContains(t, err, "no longer configured")
}

type failingConfigStore struct{}

func (failingConfigStore) ListServerConfigs(context.Context, string, map[string]any) ([]ServerConfig, error) {
	return nil, errors.New("store unavailable")
}

func TestClientErrorsAndConcurrentNamespaceIsolation(t *testing.T) {
	// Store errors abort discovery; a down connector instead contributes an unavailable status.
	_, _, err := NewClient(failingConfigStore{}).ListTools(t.Context(), "alice", nil)
	require.ErrorContains(t, err, "store unavailable")
	store := NewMemoryStore()
	require.NoError(t, store.Put(t.Context(), "alice", catalogConfig("down", "https://down.invalid/mcp")))
	statuses, tools, err := NewClient(store).ListTools(t.Context(), "alice", nil)
	require.NoError(t, err)
	require.Empty(t, tools)
	require.Equal(t, agents.ToolsetErrorUnavailable, statuses[0].Kind)

	// Same-name user servers never share private or public schema cache entries across namespaces.
	endpoint := userCatalogServer(t, "ok")
	for _, scope := range []string{"alice", "bob"} {
		config := catalogConfig("shared-name", endpoint)
		config.Meta = map[string]any{"owner": scope}
		config.CacheTTLSeconds = 60
		config.DefaultCacheScope = CacheScopePublic
		require.NoError(t, store.Put(t.Context(), scope, config))
	}
	require.NoError(t, store.Delete(t.Context(), "alice", "down"))
	cache := newMemCache()
	client := NewClient(store).WithSchemaCache(cache)
	var workers sync.WaitGroup
	for _, scope := range []string{"alice", "bob"} {
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, tools, err := client.ListTools(t.Context(), scope, nil)
			if err != nil {
				t.Error(err)
				return
			}
			for _, tool := range tools {
				if tool.GetToolDescriptor().MCPServerName != "shared-name" || tool.GetToolDescriptor().Meta["owner"] != scope {
					t.Error("cross-namespace tool")
				}
			}
		}()
	}
	workers.Wait()
	require.Len(t, cache.keys(), 2, "namespace definitions must not share even public schema entries")
}

func TestStoredOAuthBindingsAreNamespaceScoped(t *testing.T) {
	// Different users can configure the same name with different OAuth applications.
	store := NewMemoryStore()
	provider := authorizationProvider(t)
	for _, namespace := range []string{"alice", "bob"} {
		config := catalogConfig("mail", "https://mail.example/mcp")
		config.Authorization = &OAuthConfig{ClientID: namespace, RedirectURL: "https://app.example/mail/callback", AuthURL: "https://accounts.example/authorize", TokenURL: "https://accounts.example/token"}
		require.NoError(t, store.Put(t.Context(), namespace, config))
	}
	client := NewClient(store).WithCredentials(provider)
	handler := NewHandler(store, WithOAuth(provider), WithNamespaceResolver(func(r *http.Request) (string, error) { return r.Header.Get("Subject"), nil }))
	for _, namespace := range []string{"alice", "bob"} {
		statuses, tools, err := client.ListTools(t.Context(), namespace, nil)
		require.NoError(t, err)
		require.Empty(t, tools)
		require.Equal(t, agents.ToolsetErrorAuth, statuses[0].Kind)
		request := httptest.NewRequest(http.MethodGet, "/mail/connect", nil)
		request.Header.Set("Subject", namespace)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		require.Equal(t, http.StatusFound, response.Code)
		require.Contains(t, response.Header().Get("Location"), "client_id="+namespace)
	}

	// Shared authorization setup does not force public servers to use OAuth.
	public := catalogConfig("public", catalogServer(t, "public"))
	require.NoError(t, store.Put(t.Context(), "", public))
	statuses, tools, err := client.ListTools(t.Context(), "alice", nil)
	require.NoError(t, err)
	require.Len(t, tools, 3)
	require.Len(t, statuses, 2)
	require.Equal(t, "public", statuses[0].Name, "globals are listed before user connectors")
	require.True(t, statuses[0].Connected)
	require.Equal(t, agents.ToolsetErrorAuth, statuses[1].Kind)
}
