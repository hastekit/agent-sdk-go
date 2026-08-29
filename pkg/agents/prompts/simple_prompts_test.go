package prompts_test

import (
	"context"
	"strings"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/prompts"
)

func skillDeps() *agents.Dependencies {
	return &agents.Dependencies{
		Skills: []agents.Skill{{
			Name:         "changelog",
			Description:  "Write a release changelog entry.",
			FileLocation: "skills/changelog/SKILL.md",
		}},
		SkillHint: "Read one with the `read_skill` tool, passing the skill's name.",
	}
}

func TestSkillsAreListedWithTheToolThatReadsThem(t *testing.T) {
	prompt, err := prompts.New("base", prompts.WithResolver(prompts.DefaultResolvers()...)).
		GetPrompt(context.Background(), skillDeps())
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}

	for _, want := range []string{
		"read_skill",
		"<name>changelog</name>",
		"Write a release changelog entry.",
		"<location>skills/changelog/SKILL.md</location>",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q:\n%s", want, prompt)
		}
	}
}

// What the skills are and how one is read are the provider's to say, and the
// prompt repeats the hint word for word — the host serving skills through a
// tool of its own is what this is for.
func TestSkillHintIsUsedVerbatim(t *testing.T) {
	deps := &agents.Dependencies{
		Skills:    []agents.Skill{{Name: "changelog", Description: "Write a release changelog entry."}},
		SkillHint: "Read one with the `read_file` tool at the location listed below.",
	}

	prompt, err := prompts.New("base", prompts.WithResolver(prompts.ResolveSkills)).
		GetPrompt(context.Background(), deps)
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}

	if !strings.Contains(prompt, deps.SkillHint) {
		t.Errorf("prompt does not carry the hint:\n%s", prompt)
	}
	if !strings.Contains(prompt, "<location>/skills/changelog/SKILL.md</location>") {
		t.Errorf("prompt does not use the sandbox location:\n%s", prompt)
	}
}

// A provider that says nothing gets the bare catalogue and no prose invented
// for it — not a tool to reach them with (the old fallback named the bash
// tool, a lie for every host that serves its skills another way) and not a
// description of what a skill is.
func TestSkillsWithNoHintInventNone(t *testing.T) {
	deps := &agents.Dependencies{
		Skills: []agents.Skill{{Name: "changelog", Description: "Write a release changelog entry."}},
	}

	prompt, err := prompts.New("base", prompts.WithResolver(prompts.ResolveSkills)).
		GetPrompt(context.Background(), deps)
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}

	if strings.Contains(prompt, "execute_bash_commands") {
		t.Errorf("prompt invented a tool to reach the skills with:\n%s", prompt)
	}
	if want := "## Skills\n\n<available_skills>"; !strings.Contains(prompt, want) {
		t.Errorf("prompt has prose the provider did not write:\n%s", prompt)
	}
	if !strings.Contains(prompt, "<name>changelog</name>") {
		t.Errorf("skill is not listed:\n%s", prompt)
	}
}

// A caller with nothing to contribute passes nil — the summariser does.
func TestNilDependenciesResolveToThePromptItself(t *testing.T) {
	prompt, err := prompts.New("base", prompts.WithResolver(prompts.DefaultResolvers()...)).
		GetPrompt(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	if prompt != "base" {
		t.Errorf("prompt = %q", prompt)
	}
}

// Nothing is appended to a prompt that asked for no resolvers, whatever the
// run carries.
func TestAPromptWithNoResolversIsUsedAsWritten(t *testing.T) {
	prompt, err := prompts.New("base {{UserName}}").GetPrompt(context.Background(), skillDeps())
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	if prompt != "base {{UserName}}" {
		t.Errorf("prompt = %q", prompt)
	}
}

func TestResolversRunInOrderAndAccumulate(t *testing.T) {
	mark := func(s string) prompts.PromptResolverFn {
		return func(prompt string, _ *agents.Dependencies) (string, error) {
			return prompt + s, nil
		}
	}

	prompt, err := prompts.New("base",
		prompts.WithResolver(mark("-one"), mark("-two")),
		prompts.WithResolver(mark("-three")),
	).GetPrompt(context.Background(), skillDeps())
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}

	if prompt != "base-one-two-three" {
		t.Errorf("prompt = %q", prompt)
	}
}

func TestTemplateResolverFillsFromRunContext(t *testing.T) {
	deps := &agents.Dependencies{RunContext: map[string]any{"UserName": "John Doe"}}

	prompt, err := prompts.New("Hello {{UserName}}", prompts.WithResolver(prompts.ResolveTemplate)).
		GetPrompt(context.Background(), deps)
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	if prompt != "Hello John Doe" {
		t.Errorf("prompt = %q", prompt)
	}
}

func TestAResolverErrorStopsTheChain(t *testing.T) {
	boom := func(string, *agents.Dependencies) (string, error) {
		return "", context.DeadlineExceeded
	}

	if _, err := prompts.New("base", prompts.WithResolver(boom)).GetPrompt(context.Background(), nil); err == nil {
		t.Error("GetPrompt succeeded despite a failing resolver")
	}
}

func connectorDeps() *agents.Dependencies {
	return &agents.Dependencies{
		Connectors: []agents.ConnectorStatus{
			{Name: "calendar", Connected: true, ToolCount: 12},
			{Name: "jira", Kind: agents.ToolsetErrorAuth},
			{Name: "grafana", Kind: agents.ToolsetErrorUnavailable, Detail: "Service Unavailable"},
		},
	}
}

func TestConnectorsAreListedWithTheirToolCounts(t *testing.T) {
	p := prompts.New("You keep the on-call rota.", prompts.WithResolver(prompts.ResolveConnectors))

	got, err := p.GetPrompt(context.Background(), connectorDeps())
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}

	for _, want := range []string{
		"<name>calendar</name><status>connected</status><tools>12</tools>",
		"the user has to reconnect it",
		"could not be reached (Service Unavailable)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt is missing %q:\n%s", want, got)
		}
	}

	if strings.Contains(got, "<name>jira</name><status>connected</status>") {
		t.Errorf("a connector that failed must not be listed as connected:\n%s", got)
	}
}

// An auth failure is the one the user can act on, so it has to read differently
// from a server that is merely down — otherwise the model tells them to wait.
func TestConnectorReasonSeparatesAuthFromOutage(t *testing.T) {
	p := prompts.New("base", prompts.WithResolver(prompts.ResolveConnectors))

	got, err := p.GetPrompt(context.Background(), connectorDeps())
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}

	auth := strings.Index(got, "not authorized")
	outage := strings.Index(got, "could not be reached")
	if auth < 0 || outage < 0 || auth == outage {
		t.Fatalf("the two failures must read differently:\n%s", got)
	}
}

func TestNoConnectorsLeavesThePromptAlone(t *testing.T) {
	p := prompts.New("You keep the on-call rota.", prompts.WithResolver(prompts.ResolveConnectors))

	got, err := p.GetPrompt(context.Background(), &agents.Dependencies{})
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}

	if got != "You keep the on-call rota." {
		t.Errorf("an agent with no MCP servers gets no section:\n%s", got)
	}
}

func TestConnectorsAreInTheDefaultChain(t *testing.T) {
	p := prompts.New("base", prompts.WithResolver(prompts.DefaultResolvers()...))

	got, err := p.GetPrompt(context.Background(), connectorDeps())
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}

	if !strings.Contains(got, "## MCP Connectors") {
		t.Errorf("DefaultResolvers must render connectors:\n%s", got)
	}
}
