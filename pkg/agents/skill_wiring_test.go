package agents

import (
	"context"
	"strings"
	"testing"
)

func testSkillSet(policy SkillPolicy) SkillSet {
	return SkillSetFuncs{Name: "builtin", List: func(context.Context, string, map[string]any) ([]Skill, error) {
		return []Skill{{Name: "pdf", Description: "Fill and read PDF forms.", Resources: []string{"references/forms.md"}, Policy: policy}}, nil
	}, Resolve: func(_ context.Context, _ string, _ map[string]any, name, file string) (string, error) {
		if file != "" {
			return "field syntax\n", nil
		}
		return "Use pdftk for forms.", nil
	}}
}

func TestAgentBuildsOneSkillReaderPerRun(t *testing.T) {
	agent := NewAgent(&AgentOptions{Name: "skilled", Skills: []SkillSet{testSkillSet(SkillEnabled)}})
	tools, skills, hint, err := agent.prepareSkills(context.Background(), &AgentInput{}, agent.tools)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || functionName(tools[0]) != ReadSkillToolName {
		t.Fatalf("tools: %v", tools)
	}
	if len(skills) != 1 || skills[0].Name != "pdf" || !strings.Contains(hint, ReadSkillToolName) {
		t.Fatalf("skills=%v hint=%q", skills, hint)
	}
	if len(agent.tools) != 0 {
		t.Fatal("mutated shared tools")
	}
}
func TestAgentWithoutSkillsIsUnchanged(t *testing.T) {
	agent := NewAgent(&AgentOptions{Name: "plain"})
	tools, skills, hint, err := agent.prepareSkills(context.Background(), &AgentInput{}, nil)
	if err != nil || len(tools) != 0 || len(skills) != 0 || hint != "" {
		t.Fatalf("%v %v %q %v", tools, skills, hint, err)
	}
}
