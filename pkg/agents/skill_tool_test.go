package agents_test

import (
	"context"
	"strings"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

func readSkillTool(t *testing.T) *agents.ReadSkillTool {
	t.Helper()

	return agents.NewReadSkillTool(newRegistry(t, skillFS()))
}

func runReadSkill(t *testing.T, tool *agents.ReadSkillTool, args string) (string, error) {
	t.Helper()

	resp, err := tool.Execute(context.Background(), &agents.ToolCall{
		FunctionCallMessage: &responses.FunctionCallMessage{
			ID:        "fc_1",
			CallID:    "call_1",
			Name:      "read_skill",
			Arguments: args,
		},
	})
	if err != nil {
		return "", err
	}
	return *resp.Output.OfString, nil
}

func TestReadSkillReturnsInstructionsAndBundledFiles(t *testing.T) {
	out, err := runReadSkill(t, readSkillTool(t), `{"name":"pdf"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(out, "Use pdftk for forms.") {
		t.Errorf("instructions missing: %q", out)
	}
	// The model has no other way to learn what else the folder holds.
	if !strings.Contains(out, "references/forms.md") {
		t.Errorf("bundled files not listed: %q", out)
	}
}

func TestReadSkillReadsABundledFile(t *testing.T) {
	out, err := runReadSkill(t, readSkillTool(t), `{"name":"pdf","file":"references/forms.md"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out != "field syntax\n" {
		t.Errorf("content = %q", out)
	}
}

// An unknown skill comes back as a tool error, which the agent loop turns into
// a message the model can recover from rather than a failed run.
func TestReadSkillErrorsOnUnknownSkill(t *testing.T) {
	_, err := runReadSkill(t, readSkillTool(t), `{"name":"spreadsheet"}`)
	if err == nil {
		t.Fatal("Execute succeeded for an unknown skill")
	}
	if !strings.Contains(err.Error(), "pdf") {
		t.Errorf("error does not name the available skills: %v", err)
	}
}

func TestReadSkillIsAnnotatedReadOnly(t *testing.T) {
	tool := readSkillTool(t)

	if !tool.GetAnnotations().IsReadOnly() {
		t.Error("read_skill is not annotated read-only")
	}
	if tool.NeedApproval() {
		t.Error("read_skill asks for approval")
	}
	if name := tool.Tool(context.Background()).OfFunction.Name; name != "read_skill" {
		t.Errorf("tool name = %q", name)
	}
}
