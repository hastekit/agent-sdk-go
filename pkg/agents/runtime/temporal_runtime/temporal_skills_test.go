package temporal_runtime_test

import (
	"context"
	"testing"
	"testing/fstest"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/temporal_runtime"
	"github.com/hastekit/agent-sdk-go/pkg/agents/skills"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/converter"
)

func skillSource(t *testing.T) agents.SkillSet {
	t.Helper()

	registry, err := skills.NewFSSkillSet("builtin", fstest.MapFS{
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
		Skills:  []agents.SkillSet{skillSource(t)},
		History: history.NewConversationManager(history.NewInMemoryConversationPersistence()),
	}

	activities := temporal_runtime.NewTemporalAgent(nil, options, nil).GetActivities()

	assert.Contains(t, activities, "Release_Agent_SkillSet_builtin_ListSkills")
	assert.Contains(t, activities, "Release_Agent_SkillSet_builtin_ReadSkill")
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
// on the worker, so the skills and the hint introducing them have to survive
// Temporal's data converter — a section the model never sees is a skill it
// never uses.
func TestDependencies_CarrySkillsAcrossTheActivityBoundary(t *testing.T) {
	registry := skillSource(t)
	skills, err := registry.ListSkills(context.Background(), "default", nil)
	require.NoError(t, err)
	hint := "Read enabled skills using " + agents.ReadSkillToolName

	payload, err := converter.GetDefaultDataConverter().ToPayload(&agents.Dependencies{
		Skills:    skills,
		SkillHint: hint,
	})
	require.NoError(t, err)

	var got agents.Dependencies
	require.NoError(t, converter.GetDefaultDataConverter().FromPayload(payload, &got))

	require.Len(t, got.Skills, 1)
	assert.Equal(t, "changelog", got.Skills[0].Name)
	assert.Equal(t, "Write a release changelog entry.", got.Skills[0].Description)
	assert.Equal(t, "skills/changelog/SKILL.md", got.Skills[0].FileLocation)
	assert.Equal(t, hint, got.SkillHint)
	assert.Contains(t, got.SkillHint, agents.ReadSkillToolName)
}
