package agentconfig_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/hastekit/agent-sdk-go/internal/testutil"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentconfig"
	"github.com/hastekit/agent-sdk-go/pkg/agents/mcpclient"
	"github.com/hastekit/agent-sdk-go/pkg/agents/skills"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func config(name string) agentconfig.Config {
	return agentconfig.Config{Name: name, Model: "Test/m"}
}

func TestMutableStoresIsolateNamespaces(t *testing.T) {
	fileStore, err := agentconfig.NewFileStore(t.TempDir())
	require.NoError(t, err)
	for name, store := range map[string]agentconfig.MutableStore{"memory": agentconfig.NewMemoryStore(), "file": fileStore} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			stored := config("helper")
			stored.MCP = map[string]mcpclient.ToolFilter{"docs": {Include: []string{"search"}}}
			stored.Output = json.RawMessage(`{"type":"object"}`)
			_, err := store.Put(ctx, "tenant-a", stored)
			require.NoError(t, err)

			got, err := store.Get(ctx, "tenant-a", "helper")
			require.NoError(t, err)
			assert.Equal(t, stored, got)
			_, err = store.Get(ctx, "tenant-b", "helper")
			require.ErrorIs(t, err, agentconfig.ErrNotFound)
			_, err = store.Get(ctx, "", "helper")
			require.ErrorIs(t, err, agentconfig.ErrNotFound, "a namespace's config is never a built-in")

			// What the caller holds afterwards cannot change what was stored.
			stored.MCP["docs"] = mcpclient.ToolFilter{}
			got, err = store.Get(ctx, "tenant-a", "helper")
			require.NoError(t, err)
			assert.Equal(t, []string{"search"}, got.MCP["docs"].Include)

			page, err := store.List(ctx, "tenant-b", agentconfig.ListOptions{})
			require.NoError(t, err)
			assert.Empty(t, page.Agents)

			_, err = store.Put(ctx, " ", config("x"))
			require.ErrorIs(t, err, agentconfig.ErrInvalid, "a blank namespace cannot pass for the built-in one")
			_, err = store.Put(ctx, "tenant-a", agentconfig.Config{Name: "no-model"})
			require.ErrorIs(t, err, agentconfig.ErrInvalid)

			require.NoError(t, store.Delete(ctx, "tenant-a", "helper"))
			require.NoError(t, store.Delete(ctx, "tenant-a", "helper"), "delete is idempotent")
			_, err = store.Get(ctx, "tenant-a", "helper")
			require.ErrorIs(t, err, agentconfig.ErrNotFound)
		})
	}
}

func TestListPagesByName(t *testing.T) {
	store := agentconfig.NewMemoryStore()
	for i := range 5 {
		_, err := store.Put(context.Background(), "ns", config(fmt.Sprintf("agent-%d", 4-i)))
		require.NoError(t, err)
	}
	page, err := store.List(context.Background(), "ns", agentconfig.ListOptions{Limit: 2})
	require.NoError(t, err)
	require.Equal(t, []string{"agent-0", "agent-1"}, names(page.Agents))
	page, err = store.List(context.Background(), "ns", agentconfig.ListOptions{Limit: 2, Cursor: page.NextCursor})
	require.NoError(t, err)
	require.Equal(t, []string{"agent-2", "agent-3"}, names(page.Agents))
}

func names(configs []agentconfig.Config) []string {
	var out []string
	for _, c := range configs {
		out = append(out, c.Name)
	}
	return out
}

func TestInlineStoreHoldsBuiltInsOnly(t *testing.T) {
	store, err := agentconfig.NewInlineStore(config("concierge"))
	require.NoError(t, err)
	_, err = store.Get(context.Background(), "", "concierge")
	require.NoError(t, err)
	_, err = store.Get(context.Background(), "tenant-a", "concierge")
	require.ErrorIs(t, err, agentconfig.ErrNotFound)

	_, err = agentconfig.NewInlineStore(config("dup"), config("dup"))
	require.ErrorIs(t, err, agentconfig.ErrInvalid)
}

func TestCatalogPrecedence(t *testing.T) {
	ctx := context.Background()
	builtIns, err := agentconfig.NewInlineStore(config("concierge"))
	require.NoError(t, err)
	users := agentconfig.NewMemoryStore()
	shadow := config("concierge")
	shadow.Instruction = "an impostor"
	_, err = users.Put(ctx, "tenant-a", shadow)
	require.NoError(t, err)
	_, err = users.Put(ctx, "tenant-a", config("mine"))
	require.NoError(t, err)

	catalog := agentconfig.NewCatalog(builtIns, users)
	got, builtIn, err := catalog.Get(ctx, "tenant-a", "concierge")
	require.NoError(t, err)
	assert.True(t, builtIn, "a built-in wins over a namespace's config with its name")
	assert.Empty(t, got.Instruction)

	listing, err := catalog.List(ctx, "tenant-a")
	require.NoError(t, err)
	assert.Equal(t, []string{"concierge"}, names(listing.BuiltIn))
	assert.Equal(t, []string{"mine"}, names(listing.Own), "a shadowed config is left out")

	// The same name in two stores for one scope is an error, never a choice.
	twice, err := agentconfig.NewInlineStore(config("concierge"))
	require.NoError(t, err)
	_, _, err = agentconfig.NewCatalog(builtIns, twice).Get(ctx, "tenant-a", "concierge")
	require.ErrorContains(t, err, "duplicate built-in agent")
}

// A built-in hands off among built-ins only, so a namespace cannot change
// what a built-in does by creating an agent with its target's name.
func TestSnapshotKeepsBuiltInHandoffsAmongBuiltIns(t *testing.T) {
	ctx := context.Background()
	concierge := config("concierge")
	concierge.Handoffs = []agentconfig.Handoff{{Name: "billing"}}
	builtIns, err := agentconfig.NewInlineStore(concierge)
	require.NoError(t, err)
	users := agentconfig.NewMemoryStore()
	front := config("front")
	front.Handoffs = []agentconfig.Handoff{{Name: "billing"}, {Name: "concierge"}}
	for _, c := range []agentconfig.Config{front, config("billing")} {
		_, err := users.Put(ctx, "tenant-a", c)
		require.NoError(t, err)
	}
	catalog := agentconfig.NewCatalog(builtIns, users)

	snapshot, err := catalog.Snapshot(ctx, "tenant-a", "concierge")
	require.NoError(t, err)
	assert.Len(t, snapshot.Agents, 1, "the namespace's billing is not the built-in's to reach")
	assert.Empty(t, snapshot.Agents["concierge"].Handoffs)

	snapshot, err = catalog.Snapshot(ctx, "tenant-a", "front")
	require.NoError(t, err)
	assert.Len(t, snapshot.Agents, 3, "a namespace's agent reaches its own and built-ins")
	assert.Len(t, snapshot.Agents["front"].Handoffs, 2)
	assert.Empty(t, snapshot.Agents["concierge"].Handoffs, "even when the target is already in the graph")
	require.NoError(t, snapshot.Check("tenant-a"))
}

func TestRegistryReservesGlobalSkillsWithoutBuildingAgents(t *testing.T) {
	skillStore, err := skills.NewInlineStore(skills.Bundle{Files: map[string][]byte{
		"SKILL.md": []byte("---\nname: house-style\ndescription: Our tone.\n---\nBe brief."),
	}})
	require.NoError(t, err)
	store := agentconfig.NewMemoryStore()
	_, err = store.Put(context.Background(), "tenant-a", config("helper"))
	require.NoError(t, err)
	models := &countingModels{inner: agentconfig.Models{"Test/m": testutil.NewScriptedProvider()}}
	builder, err := agentconfig.NewBuilder(agentconfig.Dependencies{Stores: []agentconfig.Store{store}, Models: models, SkillStores: []skills.Store{skillStore}})
	require.NoError(t, err)

	registry := agentconfig.NewRegistry(builder)
	reserved, err := registry.GlobalSkillNames(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"house-style"}, reserved)
	assert.Zero(t, models.resolved, "listing reserved skills must not build any agent")

	names, err := registry.AgentNames(context.Background(), "tenant-a")
	require.NoError(t, err)
	assert.Equal(t, []string{"helper"}, names)
	assert.Zero(t, models.resolved, "listing agents builds nothing either")

	_, ok, err := registry.Agent(context.Background(), "tenant-b", "helper")
	require.NoError(t, err)
	assert.False(t, ok)
}

// uncomparableStore is a Store whose values cannot be compared with ==.
type uncomparableStore struct {
	agentconfig.Store
	tags []string
}

func TestRegistryReadsOnlyItsBuildersStores(t *testing.T) {
	store := agentconfig.NewMemoryStore()
	odd := uncomparableStore{Store: agentconfig.NewMemoryStore()}
	builder, err := agentconfig.NewBuilder(agentconfig.Dependencies{
		Stores: []agentconfig.Store{odd, store},
		Models: agentconfig.Models{"Test/m": testutil.NewScriptedProvider()},
	})
	require.NoError(t, err)
	registry := agentconfig.NewRegistry(builder)
	assert.True(t, registry.Reads(store))
	assert.False(t, registry.Reads(agentconfig.NewMemoryStore()))
	assert.False(t, registry.Reads(nil))
	assert.False(t, registry.Reads(uncomparableStore{Store: store}), "and comparing one never panics")
	assert.False(t, agentconfig.NewRegistry(nil).Reads(store))
}

func TestOutputMustBeASchemaObject(t *testing.T) {
	cfg := config("structured")
	cfg.Output = json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}}}`)
	require.NoError(t, agentconfig.Validate(cfg))
	schema, err := cfg.OutputSchema()
	require.NoError(t, err)
	assert.Equal(t, "object", schema["type"])

	for _, bad := range []string{`[1]`, `"schema"`, `42`} {
		cfg.Output = json.RawMessage(bad)
		require.ErrorIs(t, agentconfig.Validate(cfg), agentconfig.ErrInvalid, bad)
	}
}
