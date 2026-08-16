package temporal_runtime_test

import (
	"testing"
	"testing/fstest"

	"github.com/hastekit/hastekit-sdk-go/pkg/agents"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/history"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/runtime/temporal_runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/converter"
)

func skillRegistry(t *testing.T) *agents.SkillRegistry {
	t.Helper()

	registry, err := agents.NewSkillRegistry(fstest.MapFS{
		"skills/changelog/SKILL.md": &fstest.MapFile{Data: []byte(
			"---\nname: changelog\ndescription: Write a release changelog entry.\n---\n\nGroup by Added and Fixed.\n")},
	})
	require.NoError(t, err)

	return registry
}

// The agent adds the tool that reads its skills itself, so it never appears in
// AgentOptions.Tools. Without an activity registered under its name, the
// workflow's first read_skill call fails on an unknown activity type.
func TestGetActivities_RegistersTheSkillReaderTool(t *testing.T) {
	options := &agents.AgentOptions{
		Name:    "Release_Agent",
		Skills:  skillRegistry(t),
		History: history.NewConversationManager(history.NewInMemoryConversationPersistence()),
	}

	activities := temporal_runtime.NewTemporalAgent(nil, options, nil).GetActivities()

	assert.Contains(t, activities, "Release_Agent_read_skill_ExecuteToolActivity")
}

func TestGetActivities_RegistersNothingExtraWithoutSkills(t *testing.T) {
	options := &agents.AgentOptions{
		Name:    "Plain_Agent",
		History: history.NewConversationManager(history.NewInMemoryConversationPersistence()),
	}

	activities := temporal_runtime.NewTemporalAgent(nil, options, nil).GetActivities()

	assert.NotContains(t, activities, "Plain_Agent_read_skill_ExecuteToolActivity")
}

// The workflow builds the prompt's Dependencies and the activity renders them
// on the worker, so the skills and the name of the tool that reads them have
// to survive Temporal's data converter — a section the model never sees is a
// skill it never uses.
func TestDependencies_CarrySkillsAcrossTheActivityBoundary(t *testing.T) {
	registry := skillRegistry(t)
	skills, toolName := registry.Skills(), agents.ReadSkillToolName

	payload, err := converter.GetDefaultDataConverter().ToPayload(&agents.Dependencies{
		Skills:        skills,
		SkillToolName: toolName,
	})
	require.NoError(t, err)

	var got agents.Dependencies
	require.NoError(t, converter.GetDefaultDataConverter().FromPayload(payload, &got))

	require.Len(t, got.Skills, 1)
	assert.Equal(t, "changelog", got.Skills[0].Name)
	assert.Equal(t, "Write a release changelog entry.", got.Skills[0].Description)
	assert.Equal(t, "skills/changelog/SKILL.md", got.Skills[0].FileLocation)
	assert.Equal(t, agents.ReadSkillToolName, got.SkillToolName)
}
