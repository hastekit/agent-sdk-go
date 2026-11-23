package mcpclient

import (
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

// filterFixture stores three globals and two of alice's own servers. One of
// alice's shares its name with a global.
func filterFixture(t *testing.T) *Store {
	t.Helper()
	ctx := t.Context()
	store := NewMemoryStore()
	require.NoError(t, store.Put(ctx, "", catalogConfig("docs", catalogServer(t, "global docs"))))
	require.NoError(t, store.Put(ctx, "", catalogConfig("search", catalogServer(t, "global search"))))
	ops := catalogConfig("ops", catalogServer(t, "global ops"))
	ops.ToolFilter = ToolFilter{Include: []string{"read", "write"}}
	require.NoError(t, store.Put(ctx, "", ops))
	require.NoError(t, store.Put(ctx, "alice", catalogConfig("search", userCatalogServer(t, "alice search"))))
	require.NoError(t, store.Put(ctx, "alice", catalogConfig("notes", userCatalogServer(t, "alice notes"))))
	return store
}

func filterCall(namespace, name string) *agents.ToolCall {
	return &agents.ToolCall{Namespace: namespace, FunctionCallMessage: &responses.FunctionCallMessage{Name: name, Arguments: "{}"}}
}

func toolsByName(tools []agents.Tool) map[string]agents.Tool {
	byName := map[string]agents.Tool{}
	for _, tool := range tools {
		byName[tool.GetToolDescriptor().ToolUnion.OfFunction.Name] = tool
	}
	return byName
}

func listedFilters(t *testing.T, store MCPServerConfigStore, namespace string) map[string]ToolFilter {
	t.Helper()
	configs, err := store.ListServerConfigs(t.Context(), namespace, nil)
	require.NoError(t, err)
	listed := map[string]ToolFilter{}
	for _, config := range configs {
		listed[config.Namespace+"/"+config.Name] = config.ToolFilter
	}
	return listed
}

func TestFilterListsOnlyNamedServersAndTools(t *testing.T) {
	ctx := t.Context()
	store := filterFixture(t)
	servers := map[string]ToolFilter{
		"docs":    {},                                     // every tool
		"ops":     {Include: []string{"write", "hidden"}}, // hidden is outside the server's own filter
		"notes":   {Include: []string{"read"}},            // alice's own server
		"missing": {Include: []string{"read"}},
	}
	filtered := Filter(store, servers)
	servers["search"] = ToolFilter{} // later changes to the caller's map do not apply

	// Named servers of either scope are kept and narrowed; alice's unnamed search is dropped.
	require.Equal(t, map[string]ToolFilter{
		"/docs":       {},
		"/ops":        {Include: []string{"write", "hidden"}, Exclude: []string{"hidden"}},
		"alice/notes": {Include: []string{"read"}},
	}, listedFilters(t, filtered, "alice"))

	client := NewClient(filtered)
	statuses, tools, err := client.ListTools(ctx, "alice", nil)
	require.NoError(t, err)
	require.Equal(t, []agents.ConnectorStatus{
		agents.ConnectedConnectorStatus("docs", 3),
		agents.ConnectedConnectorStatus("ops", 1),
		agents.ConnectedConnectorStatus("notes", 1),
	}, statuses)
	byName := toolsByName(tools)
	require.Contains(t, byName, "ops__write")
	require.NotContains(t, byName, "ops__read")
	output, err := byName["ops__write"].Execute(ctx, filterCall("alice", "ops__write"))
	require.NoError(t, err)
	require.Contains(t, *output.Output.OfString, "global ops")
	output, err = byName["notes__read"].Execute(ctx, filterCall("alice", "notes__read"))
	require.NoError(t, err)
	require.Contains(t, *output.Output.OfString, "alice notes")

	// Execution re-lists through the filter, so tools and servers left out are not callable.
	unfiltered := map[string]*agents.BaseTool{}
	_, tools, err = NewClient(store).ListTools(ctx, "alice", nil)
	require.NoError(t, err)
	for name, tool := range toolsByName(tools) {
		unfiltered[name] = tool.GetToolDescriptor()
	}
	_, err = client.CallTool(ctx, unfiltered["ops__read"], filterCall("alice", "ops__read"))
	require.ErrorContains(t, err, "excluded by the current filter")
	_, err = client.CallTool(ctx, unfiltered["notes__write"], filterCall("alice", "notes__write"))
	require.ErrorContains(t, err, "excluded by the current filter")
	_, err = client.CallTool(ctx, unfiltered["search__read"], filterCall("alice", "search__read"))
	require.ErrorContains(t, err, "no longer configured")

	// No tools in common leaves the server connected with none, never all of them.
	statuses, tools, err = NewClient(Filter(store, map[string]ToolFilter{"ops": {Include: []string{"hidden"}}})).ListTools(ctx, "bob", nil)
	require.NoError(t, err)
	require.Equal(t, []agents.ConnectorStatus{agents.ConnectedConnectorStatus("ops", 0)}, statuses)
	require.Empty(t, tools)

	// An empty map lists nothing, and a nil store is reported like any nil store.
	require.Empty(t, listedFilters(t, Filter(store, nil), "alice"))
	_, _, err = NewClient(Filter(nil, map[string]ToolFilter{"docs": {}})).ListTools(ctx, "alice", nil)
	require.ErrorContains(t, err, "must not be nil")
}

func TestFilterKeepUserServers(t *testing.T) {
	ctx := t.Context()
	store := filterFixture(t)

	// Unnamed user servers are kept as they are; named ones are still narrowed.
	// The global search is left out, so it does not shadow alice's.
	filtered := Filter(store, map[string]ToolFilter{"docs": {}, "notes": {Include: []string{"read"}}}, KeepUserServers())
	require.Equal(t, map[string]ToolFilter{
		"/docs":        {},
		"alice/notes":  {Include: []string{"read"}},
		"alice/search": {},
	}, listedFilters(t, filtered, "alice"))
	_, tools, err := NewClient(filtered).ListTools(ctx, "alice", nil)
	require.NoError(t, err)
	output, err := toolsByName(tools)["search__read"].Execute(ctx, filterCall("alice", "search__read"))
	require.NoError(t, err)
	require.Contains(t, *output.Output.OfString, "alice search")

	// A kept global still shadows the user's server with the same name.
	_, tools, err = NewClient(Filter(store, map[string]ToolFilter{"search": {}}, KeepUserServers())).ListTools(ctx, "alice", nil)
	require.NoError(t, err)
	output, err = toolsByName(tools)["search__read"].Execute(ctx, filterCall("alice", "search__read"))
	require.NoError(t, err)
	require.Contains(t, *output.Output.OfString, "global search")

	// With an empty map only users' own servers remain.
	require.Equal(t, map[string]ToolFilter{"alice/notes": {}, "alice/search": {}}, listedFilters(t, Filter(store, nil, KeepUserServers()), "alice"))
}

func TestToolFilterNarrowAllowsOnlyToolsBothAllow(t *testing.T) {
	names := []string{"a", "b", "c", "d"}
	filters := []ToolFilter{
		{},
		{Include: []string{"a", "b"}},
		{Include: []string{"b", "c"}, Exclude: []string{"c"}},
		{Include: []string{"d"}},
		{Exclude: []string{"a"}},
	}
	for _, f := range filters {
		for _, g := range filters {
			narrowed := f.narrow(g)
			for _, name := range names {
				require.Equal(t, f.allows(name) && g.allows(name), narrowed.allows(name), "%+v narrowed by %+v, tool %s", f, g, name)
			}
		}
	}
}
