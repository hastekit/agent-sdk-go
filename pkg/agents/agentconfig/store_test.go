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

			// What the caller holds afterwards cannot change what was stored.
			stored.MCP["docs"] = mcpclient.ToolFilter{}
			got, err = store.Get(ctx, "tenant-a", "helper")
			require.NoError(t, err)
			assert.Equal(t, []string{"search"}, got.MCP["docs"].Include)

			listed, err := store.List(ctx, "tenant-b")
			require.NoError(t, err)
			assert.Empty(t, listed)

			// Stores hold namespaces' own agents only; built-ins are code.
			for _, namespace := range []string{"", " "} {
				_, err = store.Put(ctx, namespace, config("x"))
				require.ErrorIs(t, err, agentconfig.ErrInvalid, "namespace %q", namespace)
				_, err = store.Get(ctx, namespace, "helper")
				require.ErrorIs(t, err, agentconfig.ErrInvalid, "namespace %q", namespace)
			}
			_, err = store.Put(ctx, "tenant-a", agentconfig.Config{Name: "no-model"})
			require.ErrorIs(t, err, agentconfig.ErrInvalid)

			require.NoError(t, store.Delete(ctx, "tenant-a", "helper"))
			require.NoError(t, store.Delete(ctx, "tenant-a", "helper"), "delete is idempotent")
			_, err = store.Get(ctx, "tenant-a", "helper")
			require.ErrorIs(t, err, agentconfig.ErrNotFound)
		})
	}
}

func TestListOrdersByName(t *testing.T) {
	fileStore, err := agentconfig.NewFileStore(t.TempDir())
	require.NoError(t, err)
	for name, store := range map[string]agentconfig.MutableStore{"memory": agentconfig.NewMemoryStore(), "file": fileStore} {
		t.Run(name, func(t *testing.T) {
			for i := range 3 {
				_, err := store.Put(context.Background(), "ns", config(fmt.Sprintf("agent-%d", 2-i)))
				require.NoError(t, err)
			}
			listed, err := store.List(context.Background(), "ns")
			require.NoError(t, err)
			require.Equal(t, []string{"agent-0", "agent-1", "agent-2"}, names(listed))
		})
	}
}

func names(configs []agentconfig.Config) []string {
	var out []string
	for _, c := range configs {
		out = append(out, c.Name)
	}
	return out
}

// registryOver offers builtIns and store's agents, building with a Builder
// that allows the model config uses.
func registryOver(t *testing.T, builtIns []agentconfig.Config, store agentconfig.MutableStore) *agentconfig.Registry {
	t.Helper()
	builder, err := agentconfig.NewBuilder(agentconfig.Dependencies{
		Models: agentconfig.Models{"Test/m": testutil.NewScriptedProvider()},
	})
	require.NoError(t, err)
	registry, err := agentconfig.NewRegistry(builder, builtIns, store)
	require.NoError(t, err)
	return registry
}

func TestNewRegistryValidatesBuiltIns(t *testing.T) {
	builder, err := agentconfig.NewBuilder(agentconfig.Dependencies{
		Models: agentconfig.Models{"Test/m": testutil.NewScriptedProvider()},
	})
	require.NoError(t, err)

	_, err = agentconfig.NewRegistry(builder, []agentconfig.Config{config("dup"), config("dup")}, nil)
	require.ErrorIs(t, err, agentconfig.ErrInvalid, "a built-in name is held once")
	_, err = agentconfig.NewRegistry(builder, []agentconfig.Config{{Name: "no-model"}}, nil)
	require.ErrorIs(t, err, agentconfig.ErrInvalid)
	_, err = agentconfig.NewRegistry(nil, nil, agentconfig.NewMemoryStore())
	require.Error(t, err, "a registry needs a builder")

	// The registry keeps its own copy.
	builtIns := []agentconfig.Config{config("concierge")}
	registry, err := agentconfig.NewRegistry(builder, builtIns, nil)
	require.NoError(t, err)
	builtIns[0].Instruction = "changed afterwards"
	snapshot, err := registry.Snapshot(context.Background(), "tenant-a", "concierge")
	require.NoError(t, err)
	assert.Empty(t, snapshot.Agents["concierge"].Instruction)
}

func TestRegistryPrecedence(t *testing.T) {
	ctx := context.Background()
	users := agentconfig.NewMemoryStore()
	shadow := config("concierge")
	shadow.Instruction = "an impostor"
	_, err := users.Put(ctx, "tenant-a", shadow)
	require.NoError(t, err)
	_, err = users.Put(ctx, "tenant-a", config("mine"))
	require.NoError(t, err)

	registry := registryOver(t, []agentconfig.Config{config("concierge")}, users)
	snapshot, err := registry.Snapshot(ctx, "tenant-a", "concierge")
	require.NoError(t, err)
	assert.Empty(t, snapshot.Agents["concierge"].Instruction, "a built-in wins over a namespace's config with its name")

	choices, err := registry.Choices(ctx, "tenant-a")
	require.NoError(t, err)
	assert.Equal(t, []agentconfig.Choice{{Name: "concierge", BuiltIn: true}, {Name: "mine"}}, choices.Agents,
		"a shadowed config is left out")
}

func TestRegistryWithoutAStoreOffersBuiltInsOnly(t *testing.T) {
	ctx := context.Background()
	registry := registryOver(t, []agentconfig.Config{config("concierge")}, nil)
	names, err := registry.AgentNames(ctx, "tenant-a")
	require.NoError(t, err)
	assert.Equal(t, []string{"concierge"}, names)
	_, ok, err := registry.Agent(ctx, "tenant-a", "mine")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Nil(t, registry.Store())
}

// A built-in hands off among built-ins only, so a namespace cannot change
// what a built-in does by creating an agent with its target's name.
func TestSnapshotKeepsBuiltInHandoffsAmongBuiltIns(t *testing.T) {
	ctx := context.Background()
	concierge := config("concierge")
	concierge.Handoffs = []agentconfig.Handoff{{Name: "billing"}}
	users := agentconfig.NewMemoryStore()
	front := config("front")
	front.Handoffs = []agentconfig.Handoff{{Name: "billing"}, {Name: "concierge"}}
	for _, c := range []agentconfig.Config{front, config("billing")} {
		_, err := users.Put(ctx, "tenant-a", c)
		require.NoError(t, err)
	}
	registry := registryOver(t, []agentconfig.Config{concierge}, users)

	snapshot, err := registry.Snapshot(ctx, "tenant-a", "concierge")
	require.NoError(t, err)
	assert.Len(t, snapshot.Agents, 1, "the namespace's billing is not the built-in's to reach")
	assert.Empty(t, snapshot.Agents["concierge"].Handoffs)

	snapshot, err = registry.Snapshot(ctx, "tenant-a", "front")
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
	builder, err := agentconfig.NewBuilder(agentconfig.Dependencies{Models: models, BuiltInSkillStore: skillStore})
	require.NoError(t, err)

	registry, err := agentconfig.NewRegistry(builder, nil, store)
	require.NoError(t, err)
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
