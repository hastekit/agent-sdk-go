package routines_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hastekit/agent-sdk-go/internal/testutil"
	"github.com/hastekit/agent-sdk-go/pkg/routines"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A routine can target its namespace's own agents — ones created at run time
// as configs — and no other namespace can target them.
func TestRoutinesTargetANamespacesOwnAgents(t *testing.T) {
	f := testutil.NewAgentFixture(t, nil, nil, testutil.Step{Text: "summary ready"})
	f.AddConfig(t)
	store, err := routines.OpenJSONLStore(filepath.Join(t.TempDir(), "routines.jsonl"))
	require.NoError(t, err)
	defer store.Close()
	executor := &routines.AgentExecutor{Registry: f.Registry}
	service := routines.NewService(store, executor)
	ctx := context.Background()

	definition := routines.Definition{
		Name: "daily summary", Agent: testutil.FixtureAgent, Instruction: "summarise yesterday",
		Schedule: routines.Schedule{Cron: "0 9 * * *", Timezone: "UTC"},
	}
	routine, err := service.Create(ctx, testutil.FixtureNamespace, definition)
	require.NoError(t, err)
	_, err = service.Create(ctx, "tenant-b", definition)
	require.ErrorIs(t, err, routines.ErrInvalid, "another namespace cannot target the agent")

	names, err := executor.AgentNames(ctx, testutil.FixtureNamespace)
	require.NoError(t, err)
	assert.Contains(t, names, testutil.FixtureAgent)
	names, err = executor.AgentNames(ctx, "tenant-b")
	require.NoError(t, err)
	assert.NotContains(t, names, testutil.FixtureAgent)

	require.NoError(t, executor.Execute(ctx, routine, routines.Run{ID: "occurrence-1"}))
	seen := f.Provider.Seen()
	require.Len(t, seen, 1)
	assert.Contains(t, strings.Join(seen[0].Texts, "\n"), "summarise yesterday")

	// Deleting the config leaves the routine pointing at nothing, which fails
	// the occurrence rather than running something else.
	require.NoError(t, f.Store.Delete(ctx, testutil.FixtureNamespace, testutil.FixtureAgent))
	require.ErrorContains(t, executor.Execute(ctx, routine, routines.Run{ID: "occurrence-2"}), "no longer available")
}

// The routines API offers each namespace the agents it can target.
func TestRoutineAgentsRouteListsTheNamespacesAgents(t *testing.T) {
	f := testutil.NewAgentFixture(t, nil, nil)
	f.AddConfig(t)
	store, err := routines.OpenJSONLStore(filepath.Join(t.TempDir(), "routines.jsonl"))
	require.NoError(t, err)
	defer store.Close()
	service := routines.NewService(store, &routines.AgentExecutor{Registry: f.Registry})
	server := httptest.NewServer(routines.NewHTTPHandler(service, routines.HTTPConfig{
		NamespaceResolver: func(r *http.Request) (string, error) { return r.Header.Get("X-Tenant"), nil },
	}))
	defer server.Close()

	list := func(tenant string) []string {
		req, err := http.NewRequest("GET", server.URL+"/routines/agents", nil)
		require.NoError(t, err)
		req.Header.Set("X-Tenant", tenant)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var names []string
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&names))
		return names
	}
	assert.Equal(t, []string{testutil.FixtureAgent}, list(testutil.FixtureNamespace))
	assert.Empty(t, list("tenant-b"))
}
