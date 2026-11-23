package agents_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hastekit/agent-sdk-go/internal/testutil"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentFactory_LoadSpecReturnsTheStoredVersionOrTheLatest(t *testing.T) {
	store := testutil.NewSpecStore()
	store.Put(testutil.Spec("support", "1", "first"))
	store.Put(testutil.Spec("support", "2", "second"))
	factory := store.Factory(nil)

	spec, err := factory.LoadSpec(t.Context(), agents.AgentRef{Name: "support", Version: "1"})
	require.NoError(t, err)
	assert.Equal(t, "first", spec.Instruction)

	spec, err = factory.LoadSpec(t.Context(), agents.AgentRef{Name: "support"})
	require.NoError(t, err)
	assert.Equal(t, "2", spec.Version, "an empty version is the loader's latest")

	_, err = factory.LoadSpec(t.Context(), agents.AgentRef{Name: "nobody"})
	require.ErrorIs(t, err, agents.ErrAgentNotFound)
}

func TestAgentFactory_LoadSpecRejectsSpecsItCannotBuild(t *testing.T) {
	cases := map[string]struct {
		ref  agents.AgentRef
		spec agents.AgentSpec
	}{
		"another agent":   {agents.AgentRef{Name: "a"}, testutil.Spec("b", "1", "x")},
		"another version": {agents.AgentRef{Name: "a", Version: "2"}, testutil.Spec("a", "1", "x")},
		"no model":        {agents.AgentRef{Name: "a"}, agents.AgentSpec{Name: "a"}},
		"unknown tool":    {agents.AgentRef{Name: "a"}, testutil.Spec("a", "1", "x", "lookup", "deleted_tool")},
		"no name":         {agents.AgentRef{}, testutil.Spec("", "", "x")},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			factory := &agents.AgentFactory{
				Load:   func(context.Context, agents.AgentRef) (*agents.AgentSpec, error) { return &tc.spec, nil },
				Models: testutil.EchoModels(),
				Tools:  []agents.Tool{testutil.NewTextTool("lookup", "x")},
			}
			_, err := factory.LoadSpec(t.Context(), tc.ref)
			require.ErrorIs(t, err, agents.ErrInvalidAgentSpec)
		})
	}
}

func TestAgentFactory_ValidateNeedsALoaderModelsAndUniqueToolNames(t *testing.T) {
	load := testutil.NewSpecStore().Load
	models := testutil.EchoModels()
	lookup := testutil.NewTextTool("lookup", "x")

	require.NoError(t, (&agents.AgentFactory{Load: load, Models: models, Tools: []agents.Tool{lookup}}).Validate())
	require.Error(t, (*agents.AgentFactory)(nil).Validate())
	require.Error(t, (&agents.AgentFactory{Models: models}).Validate())
	require.Error(t, (&agents.AgentFactory{Load: load}).Validate())
	require.Error(t, (&agents.AgentFactory{Load: load, Models: models, Tools: []agents.Tool{lookup, testutil.NewTextTool("lookup", "y")}}).Validate())
}

// A spec is a durable run's input, so it must survive a JSON round trip unchanged.
func TestAgentSpec_SurvivesAJSONRoundTrip(t *testing.T) {
	temperature := 0.2
	spec := agents.AgentSpec{
		Name: "support", Version: "7", Model: "OpenAI/gpt-4.1-mini", Instruction: "Help.",
		Parameters: responses.Parameters{Temperature: &temperature},
		Output:     map[string]any{"type": "object"},
		MaxLoops:   4,
		Tools:      []string{"lookup"},
		Handoffs:   []agents.HandoffSpec{{Name: "billing", Version: "3", Description: "Billing"}},
	}
	data, err := json.Marshal(spec)
	require.NoError(t, err)
	var decoded agents.AgentSpec
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, spec, decoded)
	assert.Equal(t, agents.AgentRef{Name: "billing", Version: "3"}, decoded.Handoffs[0].Ref())
}
