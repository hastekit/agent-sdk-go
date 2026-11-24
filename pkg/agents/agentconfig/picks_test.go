package agentconfig_test

import (
	"context"
	"sort"
	"testing"

	"github.com/hastekit/agent-sdk-go/internal/testutil"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentconfig"
	"github.com/hastekit/agent-sdk-go/pkg/agents/mcpclient"
	"github.com/hastekit/agent-sdk-go/pkg/agents/skills"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func skillFile(name string) skills.Bundle {
	return skills.Bundle{Files: map[string][]byte{"SKILL.md": []byte("---\nname: " + name + "\ndescription: " + name + "\n---\nbody")}}
}

// picksBuilder offers configs two global skills and servers, and gives
// tenant-a two skills and servers of its own.
func picksBuilder(t *testing.T) *agentconfig.Builder {
	t.Helper()
	ctx := context.Background()
	globalSkills, err := skills.NewInlineStore(skillFile("house-style"), skillFile("legal-review"))
	require.NoError(t, err)
	userSkills, err := skills.NewFileStore(t.TempDir())
	require.NoError(t, err)
	for _, name := range []string{"my-notes", "my-extra"} {
		_, err := userSkills.Put(ctx, "tenant-a", skillFile(name))
		require.NoError(t, err)
	}
	// Nothing listens at these endpoints: a server a run lists still reports a
	// status, so the statuses show which servers the agent has.
	globalServers := mcpclient.NewInlineStore(
		mcpclient.ServerConfig{Name: "docs", Endpoint: "http://127.0.0.1:1/mcp", Transport: mcpclient.TransportStreamableHTTP},
		mcpclient.ServerConfig{Name: "billing", Endpoint: "http://127.0.0.1:1/mcp", Transport: mcpclient.TransportStreamableHTTP},
	)
	userServers := mcpclient.NewMemoryStore()
	for _, name := range []string{"notes", "extra"} {
		require.NoError(t, userServers.Put(ctx, "tenant-a", mcpclient.ServerConfig{Name: name, Endpoint: "https://" + name + ".invalid/mcp", Transport: mcpclient.TransportStreamableHTTP}))
	}
	builder, err := agentconfig.NewBuilder(agentconfig.Dependencies{
		Models:            agentconfig.Models{testutil.FixtureModel: testutil.NewScriptedProvider()},
		BuiltInSkillStore: globalSkills,
		UserSkillStore:    userSkills,
		BuiltInMCPStore:   globalServers,
		UserMCPStore:      userServers,
	})
	require.NoError(t, err)
	return builder
}

// A config agent has its picks and nothing else: built-in or the user's own,
// every one always on. Neither an unpicked built-in nor the user's other
// skills are the agent's.
func TestPickedSkillsAreTheAgentsOnlySkillsAndAlwaysOn(t *testing.T) {
	builder := picksBuilder(t)
	agent := agents.NewAgent(&agents.AgentOptions{Name: "a", SkillClient: builder.SkillClient([]string{"house-style", "my-notes"})})

	listed, err := agent.ListSkills(context.Background(), "tenant-a", nil, agents.SkillSelection{Disable: []string{"house-style", "my-notes", "my-extra"}})
	require.NoError(t, err)
	enabled := map[string]bool{}
	global := map[string]bool{}
	for _, skill := range listed {
		enabled[skill.Name] = skill.Enabled
		global[skill.Name] = skill.Global
	}
	assert.Equal(t, map[string]bool{"house-style": true, "my-notes": true}, enabled, "picks stay on whatever a run selects")
	assert.Equal(t, map[string]bool{"house-style": true, "my-notes": true}, global)

	// Picking nothing gives the agent no skills at all.
	assert.Nil(t, builder.SkillClient(nil))
}

func TestPickedConnectorsAreTheAgentsOnlyServersAndAlwaysOn(t *testing.T) {
	builder := picksBuilder(t)
	client := builder.MCPClient(map[string]mcpclient.ToolFilter{"docs": {}, "notes": {}})
	names := func(selection agents.MCPSelection) []string {
		statuses, _, err := client.ListTools(context.Background(), "tenant-a", nil, selection)
		require.NoError(t, err)
		var listed []string
		for _, status := range statuses {
			listed = append(listed, status.Name)
		}
		sort.Strings(listed)
		return listed
	}
	assert.Equal(t, []string{"docs", "notes"}, names(agents.MCPSelection{}), "only the picks: no unpicked built-in, no other user server")
	assert.Equal(t, []string{"docs", "notes"}, names(agents.MCPSelection{Disable: []string{"docs", "notes", "extra"}}),
		"a run cannot turn a pick off")

	// Picking nothing gives the agent no servers at all.
	assert.Nil(t, builder.MCPClient(nil))
}

// Listed without connecting, a config agent's servers are its picks, all
// always on.
func TestConfigAgentsListTheirServersWithPicksAlwaysOn(t *testing.T) {
	builder := picksBuilder(t)
	client := builder.MCPClient(map[string]mcpclient.ToolFilter{"docs": {}, "notes": {}})
	lister, ok := client.(mcpclient.ServerLister)
	require.True(t, ok)
	listed, err := lister.ListServers(context.Background(), "tenant-a", nil)
	require.NoError(t, err)
	assert.Equal(t, []mcpclient.ListedServer{{Name: "docs", AlwaysOn: true}, {Name: "notes", AlwaysOn: true}}, listed)
}
