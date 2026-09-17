package skills_test

import (
	"context"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/skills"
	"github.com/stretchr/testify/require"
)

type promptRecorder struct{ skills []agents.Skill }

func (p *promptRecorder) GetPrompt(_ context.Context, d *agents.Dependencies) (string, error) {
	p.skills = d.Skills
	return "Use your skills", nil
}

type doneLLM struct{}

func (doneLLM) NewStreamingResponses(_ context.Context, _ *agents.ModelCall, _ *responses.Request, _ func(*responses.ResponseChunk)) (*responses.Response, error) {
	return &responses.Response{}, nil
}

func TestAgentBindsStoreToInputNamespace(t *testing.T) {
	store, err := skills.NewFileStore(t.TempDir())
	require.NoError(t, err)
	_, err = store.Put(context.Background(), "tenant", skills.Bundle{Files: map[string][]byte{"SKILL.md": []byte("---\nname: review\ndescription: Review work\n---\nInstructions")}})
	require.NoError(t, err)
	source, err := skills.NewSkillSet("library", store, skills.WithDefaultPolicy(agents.SkillEnabled))
	require.NoError(t, err)
	prompt := &promptRecorder{}
	agent := agents.NewAgent(&agents.AgentOptions{Name: "tester", Skills: []agents.SkillSet{source}, Instruction: prompt}).WithLLM(doneLLM{})
	// A caller-supplied RunContext namespace must not override the execution's namespace.
	_, err = agent.Run(context.Background(), &agents.AgentInput{Namespace: "tenant", RunContext: map[string]any{"Namespace": "other"}})
	require.NoError(t, err)
	require.Len(t, prompt.skills, 1)
	require.Equal(t, "review", prompt.skills[0].Name)
}
