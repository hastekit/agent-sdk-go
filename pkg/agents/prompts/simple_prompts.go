package prompts

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"text/template"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

var tracer = otel.Tracer("PromptManager")

type PromptLoader interface {
	// LoadPrompt loads the prompt from the source and returns it as string
	LoadPrompt(ctx context.Context) (string, error)
}

// PromptResolverFn is one pass over the prompt. Each receives what the last
// one produced along with the run's dependencies — the skills the agent was
// given, its handoffs, its deferred tools, the run context — and returns the
// prompt to hand on.
//
// Building the system prompt out of these is what lets a caller reorder,
// replace, or add a section without the prompt type having to know about it.
type PromptResolverFn func(prompt string, deps *agents.Dependencies) (string, error)

type StringLoader struct {
	String string
}

func NewStringLoader(str string) *StringLoader {
	return &StringLoader{
		String: str,
	}
}

func (sl *StringLoader) LoadPrompt(ctx context.Context) (string, error) {
	return sl.String, nil
}

type SimplePrompt struct {
	loader    PromptLoader
	resolvers []PromptResolverFn
}

func New(prompt string, opts ...PromptOption) *SimplePrompt {
	return NewWithLoader(NewStringLoader(prompt), opts...)
}

func NewWithLoader(loader PromptLoader, opts ...PromptOption) *SimplePrompt {
	sp := &SimplePrompt{
		loader:    loader,
		resolvers: []PromptResolverFn{},
	}

	for _, op := range opts {
		op(sp)
	}

	return sp
}

type PromptOption func(*SimplePrompt)

// WithResolver adds resolvers to the chain, run in the order given. Call it
// more than once and they accumulate, so a prompt can be assembled from a
// standard set plus one of its own:
//
//	prompts.New("You help maintain this project's releases.",
//		prompts.WithResolver(prompts.DefaultResolvers()...),
//		prompts.WithResolver(houseRules),
//	)
//
// A prompt with no resolvers is used exactly as written: nothing is appended
// to it and its {{ placeholders }} are left alone.
func WithResolver(resolvers ...PromptResolverFn) PromptOption {
	return func(sp *SimplePrompt) {
		sp.resolvers = append(sp.resolvers, resolvers...)
	}
}

// DefaultResolvers is the standard chain: the sections the agent contributes,
// then the template pass over the whole thing — so a placeholder inside a
// skill description or a handoff's is resolved too. Nothing applies these on
// your behalf; pass them to WithResolver, in this order or your own.
func DefaultResolvers() []PromptResolverFn {
	return []PromptResolverFn{
		ResolveSkills,
		ResolveHandoffs,
		ResolveDeferredTools,
		ResolveTemplate,
	}
}

func (sp *SimplePrompt) GetPrompt(ctx context.Context, deps *agents.Dependencies) (string, error) {
	ctx, span := tracer.Start(ctx, "Prompt.Load")
	defer span.End()

	prompt, err := sp.loader.LoadPrompt(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}

	// A caller with nothing to contribute (the summariser, say) passes nil.
	// Standing in an empty set keeps every resolver on one code path.
	if deps == nil {
		deps = &agents.Dependencies{}
	}

	for _, resolve := range sp.resolvers {
		if resolve == nil {
			continue
		}

		prompt, err = resolve(prompt, deps)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return "", err
		}
	}

	return prompt, nil
}

func stringToTemplate(promptStr string) (*template.Template, error) {
	re := regexp.MustCompile(`{{(\w.+)}}`)
	promptStr = re.ReplaceAllString(promptStr, "{{ .$1 }}")

	return template.New("file_prompt").Parse(promptStr)
}

// DefaultResolver resolves {{ placeholders }} against a plain map. It is the
// templating on its own, for callers that want it outside a resolver chain.
func DefaultResolver(prompt string, data map[string]any) (string, error) {
	tmpl, err := stringToTemplate(prompt)
	if err != nil {
		return prompt, err
	}

	return utils.ExecuteTemplate(tmpl, data)
}

// ResolveTemplate fills the prompt's {{ placeholders }} from the run context.
// A run that carries no context leaves the prompt alone, so a prompt that
// happens to contain braces is not a startup failure for runs that never
// meant to template it.
func ResolveTemplate(prompt string, deps *agents.Dependencies) (string, error) {
	if deps.RunContext == nil {
		return prompt, nil
	}

	return DefaultResolver(prompt, deps.RunContext)
}

// ResolveSkills appends the catalogue of skills the agent was given: the
// provider's hint, then each skill's name, description and location. Only that
// much — the instructions themselves are what the reader tool is for, and
// putting them here would cost context on every turn instead of the turns that
// use them.
func ResolveSkills(prompt string, deps *agents.Dependencies) (string, error) {
	if len(deps.Skills) == 0 {
		return prompt, nil
	}

	var p strings.Builder

	p.WriteString("\n\n" + "## Skills\n\n")
	// Verbatim from the provider, and the only prose in this section. What
	// these skills are and how one is read are both the provider's to say — it
	// serves them, through a tool of its own, or a sandbox the agent browses,
	// or a tool the host wired up — and a sentence written here could only
	// ever guess between them.
	if deps.SkillHint != "" {
		p.WriteString(deps.SkillHint + "\n")
	}
	p.WriteString("<available_skills>")
	for _, skill := range deps.Skills {
		p.WriteString("<skill>")

		p.WriteString(fmt.Sprintf("<name>%s</name>", skill.Name))
		p.WriteString(fmt.Sprintf("<description>%s</description>", skill.Description))
		p.WriteString(fmt.Sprintf("<location>%s</location>", skillLocation(skill)))

		p.WriteString("</skill>")
	}
	p.WriteString("</available_skills>")

	return prompt + p.String(), nil
}

// skillLocation keeps the sandbox convention for skills that carry no location
// of their own, so skills staged into a sandbox still point where they used to.
func skillLocation(skill agents.Skill) string {
	if skill.FileLocation != "" {
		return skill.FileLocation
	}
	return fmt.Sprintf("/skills/%s/SKILL.md", skill.Name)
}

// ResolveHandoffs appends the agents this one can transfer to.
func ResolveHandoffs(prompt string, deps *agents.Dependencies) (string, error) {
	if len(deps.Handoffs) == 0 {
		return prompt, nil
	}

	var p strings.Builder

	p.WriteString("\n\n" + "## Agents\n\n")
	p.WriteString("Agents are specialized in certain tasks or domain. Use the `transfer_to_agent` tool to delegate or transfer to the specialized agents, based on the task at hand.\n")
	p.WriteString("<available_agents>")
	for _, handoff := range deps.Handoffs {
		p.WriteString("<agent>")

		p.WriteString(fmt.Sprintf("<name>%s</name>", handoff.Name))
		p.WriteString(fmt.Sprintf("<description>%s</description>", handoff.Description))

		p.WriteString("</agent>")
	}
	p.WriteString("</available_agents>")
	p.WriteString("\n---\n")

	return prompt + p.String(), nil
}

// ResolveDeferredTools appends the tools the model must activate before it can
// call them.
func ResolveDeferredTools(prompt string, deps *agents.Dependencies) (string, error) {
	if len(deps.DeferredTools) == 0 {
		return prompt, nil
	}

	var p strings.Builder

	p.WriteString("\n\n" + "## Deferred Tools\n")
	p.WriteString("Deferred tools are tools that are not available in the current context. Use the `ToolSearch` tool to activate the deferred tool and get its full description and schema. \n")
	p.WriteString("<available-deferred-tools>")
	for _, tool := range deps.DeferredTools {
		p.WriteString(fmt.Sprintf("<deferred-tool><name>%s</name><description>%s</description></deferred-tool>", tool.Name, tool.Description))
	}
	p.WriteString("</available-deferred-tools>")
	p.WriteString("\n---\n")

	return prompt + p.String(), nil
}
