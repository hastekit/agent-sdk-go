package agents

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
)

func testRegistry(t *testing.T) *SkillRegistry {
	t.Helper()

	registry, err := NewSkillRegistry(fstest.MapFS{
		"skills/pdf/SKILL.md": &fstest.MapFile{Data: []byte(
			"---\nname: pdf\ndescription: Fill and read PDF forms.\n---\n\nUse pdftk for forms.\n")},
	})
	if err != nil {
		t.Fatalf("NewSkillRegistry: %v", err)
	}
	return registry
}

// An agent given skills gets the tool that reads them without being handed it
// separately: the prompt and the tools cannot end up disagreeing about whether
// the model can open a skill.
func TestAgentAddsTheSkillReaderTool(t *testing.T) {
	callerTools := []Tool{}

	agent := NewAgent(&AgentOptions{
		Name:   "Skilled_Agent",
		Skills: testRegistry(t),
		Tools:  callerTools,
	})

	if len(agent.tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(agent.tools))
	}
	if name := agent.tools[0].Tool(context.Background()).OfFunction.Name; name != ReadSkillToolName {
		t.Errorf("tool name = %q", name)
	}
	if len(callerTools) != 0 {
		t.Errorf("the caller's slice was appended to: %v", callerTools)
	}
}

// The prompt introduces the skills in the provider's own words, rather than
// assuming. A registry serves its own, so it names its own tool.
func TestSkillDependenciesCarryTheProvidersHint(t *testing.T) {
	skills, hint := skillDependencies(testRegistry(t))

	if len(skills) != 1 || skills[0].Name != "pdf" {
		t.Errorf("skills = %+v", skills)
	}
	if !strings.Contains(hint, ReadSkillToolName) {
		t.Errorf("hint = %q, want it to name %s", hint, ReadSkillToolName)
	}
}

// A host that serves skills its own way says so in its own words, and the
// prompt repeats them rather than guessing at a tool.
func TestSkillsWithHintCarryTheHostsWording(t *testing.T) {
	provider := SkillsWithHint{
		List: SkillList{{Name: "pdf", Description: "Fill and read PDF forms."}},
		Hint: "Read one with the `read_file` tool at the location listed below.",
	}

	agent := NewAgent(&AgentOptions{Name: "Hosted_Agent", Skills: provider})
	if len(agent.tools) != 0 {
		t.Errorf("got %d tools, want none", len(agent.tools))
	}

	skills, hint := skillDependencies(provider)
	if len(skills) != 1 {
		t.Errorf("skills = %+v", skills)
	}
	if hint != provider.Hint {
		t.Errorf("hint = %q, want %q", hint, provider.Hint)
	}
}

// A bare list is skills the agent already reaches some other way. It adds no
// tool, and says nothing about what they are or how to read them — the SDK has
// no way to know, and a guess would sooner or later name a tool the agent lacks.
func TestSkillsTheAgentAlreadyReachesAddNoTool(t *testing.T) {
	list := SkillList{{Name: "pdf", Description: "Fill and read PDF forms."}}

	agent := NewAgent(&AgentOptions{Name: "Sandboxed_Agent", Skills: list})
	if len(agent.tools) != 0 {
		t.Errorf("got %d tools, want none", len(agent.tools))
	}

	skills, hint := skillDependencies(list)
	if len(skills) != 1 {
		t.Errorf("skills = %+v", skills)
	}
	if hint != "" {
		t.Errorf("hint = %q, want empty", hint)
	}
}

// What a durable runtime does: take the effective tool list, wrap each tool as
// a workflow step, then rebuild the agent from the same options. The agent has
// to see its reader tool already there and leave it alone — a second, raw copy
// would read outside the workflow's journal, and a duplicate tool name is one
// the model can call either way.
func TestTheSkillToolIsNotAddedOverAWrappedOne(t *testing.T) {
	registry := testRegistry(t)
	options := &AgentOptions{Name: "Durable_Agent", Skills: registry}

	var wrapped []Tool
	for _, tool := range WithSkillTool(options.Tools, options.Skills) {
		wrapped = append(wrapped, &wrappedTool{inner: tool})
	}

	agent := NewAgent(&AgentOptions{
		Name:   options.Name,
		Skills: options.Skills,
		Tools:  wrapped,
	})

	if len(agent.tools) != 1 {
		t.Fatalf("got %d tools, want 1: the reader tool was added over the wrapped one", len(agent.tools))
	}
	if _, ok := agent.tools[0].(*wrappedTool); !ok {
		t.Errorf("tool is %T, want the wrapped one", agent.tools[0])
	}
}

// wrappedTool stands in for a runtime's tool proxy: same schema, different
// implementation.
type wrappedTool struct{ inner Tool }

func (w *wrappedTool) Execute(ctx context.Context, params *ToolCall) (*ToolCallResponse, error) {
	return w.inner.Execute(ctx, params)
}
func (w *wrappedTool) Tool(ctx context.Context) *responses.ToolUnion { return w.inner.Tool(ctx) }
func (w *wrappedTool) NeedApproval() bool                            { return w.inner.NeedApproval() }
func (w *wrappedTool) IsDeferred() bool                              { return w.inner.IsDeferred() }

func TestAgentWithoutSkillsIsUnchanged(t *testing.T) {
	agent := NewAgent(&AgentOptions{Name: "Plain_Agent"})
	if len(agent.tools) != 0 {
		t.Errorf("got %d tools, want none", len(agent.tools))
	}

	skills, hint := skillDependencies(nil)
	if skills != nil || hint != "" {
		t.Errorf("skills = %+v, hint = %q", skills, hint)
	}
}
