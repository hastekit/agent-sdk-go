package agents_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/prompts"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

func TestDynamicSkillRunAdvertisesAndReadsOneSnapshot(t *testing.T) {
	set := &runSkillClient{t: t}
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("read1", "read_skill", `{"name":"review"}`),
		toolCallResponse("read2", "read_skill", `{"name":"review","file":"check.md"}`),
		textResponse("ready"),
	}}
	agent := agents.NewAgent(&agents.AgentOptions{Name: "reviewer", SkillClient: set, Instruction: prompts.New("Use skills.", prompts.WithResolver(prompts.DefaultResolvers()...))}).WithLLM(llm)
	out := runAgent(t, agent, &agents.AgentInput{Namespace: "test", ThreadID: "dynamic-skills", Message: userMessage("review the release"), Skills: agents.SkillSelection{Disable: []string{"secret"}}})
	requireStatus(t, out, agentstate.RunStatusCompleted)
	require.Equal(t, 1, set.lists)
	require.Equal(t, 2, set.reads)
	encoded, err := json.Marshal(llm.request(0))
	require.NoError(t, err)
	require.Contains(t, string(encoded), "review")
	require.NotContains(t, string(encoded), "secret")
	encoded, err = json.Marshal(llm.request(2))
	require.NoError(t, err)
	require.Contains(t, string(encoded), "review instructions")
}

type runSkillClient struct {
	t            *testing.T
	lists, reads int
}

func (s *runSkillClient) ListSkills(context.Context, string, map[string]any) ([]agents.Skill, error) {
	s.lists++
	return []agents.Skill{{Name: "review", Description: "Review releases", Resources: []string{"check.md"}}, {Name: "secret", Description: "Hidden by the run's selection"}}, nil
}

func (s *runSkillClient) ReadSkill(_ context.Context, namespace string, _ map[string]any, name, file string) (string, error) {
	s.reads++
	require.Equal(s.t, "review", name)
	return "review instructions", nil
}
