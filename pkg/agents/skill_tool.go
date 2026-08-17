package agents

import (
	"context"
	"fmt"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

// ReadSkillToolName is the name the model calls to read a skill. The prompt
// names the same tool, so the two never drift apart.
const ReadSkillToolName = "read_skill"

// ReadSkillTool serves the skills in a registry. The prompt lists each skill's
// name and description; this tool is how the model pulls in the instructions
// themselves, so the full text costs context only on the turn it is needed.
//
// An agent given a SkillProvider gets this tool automatically — see
// AgentOptions.Skills. Build it directly only to hand the same registry to
// something that isn't an agent.
type ReadSkillTool struct {
	*BaseTool
	registry *SkillRegistry
}

type ReadSkillInput struct {
	Name string `json:"name"`
	File string `json:"file,omitempty"`
}

func NewReadSkillTool(registry *SkillRegistry) *ReadSkillTool {
	return &ReadSkillTool{
		BaseTool: &BaseTool{
			ToolUnion: responses.ToolUnion{
				OfFunction: &responses.FunctionTool{
					Name:        ReadSkillToolName,
					Description: utils.Ptr("Read a skill's instructions. Call this with the name of a skill listed in <available_skills> before doing the kind of work that skill covers. A skill may bundle extra files (references, scripts); read one by passing its path in `file`."),
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"name": map[string]string{
								"type":        "string",
								"description": "Name of the skill to read, exactly as listed in <available_skills>",
							},
							"file": map[string]string{
								"type":        "string",
								"description": "Optional path of a bundled file to read instead, exactly as listed at the end of that skill's instructions. Omit this to read the instructions, which is what tells you whether there is anything else worth reading.",
							},
						},
						"required": []string{"name"},
					},
				},
			},
			RequiresApproval: false,
			// Reading a skill touches nothing and reaches nowhere outside the
			// skills themselves, so a permission policy can let it run
			// unattended.
			Annotations: &ToolAnnotations{
				Title:           "Read skill",
				ReadOnlyHint:    utils.Ptr(true),
				DestructiveHint: utils.Ptr(false),
				IdempotentHint:  utils.Ptr(true),
				OpenWorldHint:   utils.Ptr(false),
			},
		},
		registry: registry,
	}
}

func (t *ReadSkillTool) Execute(ctx context.Context, params *ToolCall) (*ToolCallResponse, error) {
	var in ReadSkillInput
	err := sonic.Unmarshal([]byte(params.Arguments), &in)
	if err != nil {
		return nil, err
	}

	if t.registry == nil {
		return nil, fmt.Errorf("no skills are loaded")
	}

	name := strings.TrimSpace(in.Name)

	var output string
	if strings.TrimSpace(in.File) != "" {
		content, err := t.registry.ReadFile(name, in.File)
		if err != nil {
			return nil, err
		}
		output = content
	} else {
		content, err := t.registry.Read(name)
		if err != nil {
			return nil, err
		}
		skill, _ := t.registry.Get(name)
		output = withResourceIndex(skill, content)
	}

	return &ToolCallResponse{
		FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			ID:     params.ID,
			CallID: params.CallID,
			Output: responses.FunctionCallOutputContentUnion{
				OfString: utils.Ptr(output),
			},
		},
	}, nil
}

// WithSkillTool adds the skill source's reader tool to the agent's own,
// returning the tools an agent given both will actually run. NewAgent applies
// it, so a caller never needs to.
//
// The durable runtimes do: they register and wrap each of an agent's tools
// themselves, from AgentOptions rather than from the built agent. A tool they
// never saw is one the workflow cannot call — under Temporal it has no
// activity, and under either runtime a tool that slipped past the wrapping
// would run inline in the workflow, unjournaled. So the list they walk has to
// be this one, not opts.Tools.
//
// It copies rather than appending in place: the caller's slice is theirs, and
// an agent built from the same options twice must not accumulate the tool.
func WithSkillTool(tools []Tool, provider SkillProvider) []Tool {
	if provider == nil {
		return tools
	}

	tool := provider.SkillTool()
	if tool == nil {
		// The skills are read some other way — a sandbox the agent already
		// browses with its bash tool.
		return tools
	}

	// A durable runtime hands back a list it built from this same call, with the
	// reader tool already wrapped as a workflow step, and then rebuilds the
	// agent from the same options. Keep the one that is there: replacing a
	// proxy with a raw tool would run the read outside the workflow's journal.
	name := functionToolName(tool)
	for _, existing := range tools {
		if functionToolName(existing) == name {
			return tools
		}
	}

	withTool := make([]Tool, 0, len(tools)+1)
	withTool = append(withTool, tools...)

	return append(withTool, tool)
}

func functionToolName(tool Tool) string {
	schema := tool.Tool(context.Background())
	if schema == nil || schema.OfFunction == nil {
		return ""
	}
	return schema.OfFunction.Name
}

// skillDependencies flattens a skill source into what the prompt needs: the
// list to advertise, and the provider's own hint introducing it.
func skillDependencies(provider SkillProvider) ([]Skill, string) {
	if provider == nil {
		return nil, ""
	}

	skills := provider.Skills()
	if len(skills) == 0 {
		return nil, ""
	}

	return skills, provider.SkillHint()
}

// withResourceIndex appends the skill's bundled files to its instructions.
// The SKILL.md usually names the ones that matter, but listing them here is
// what makes them reachable when it doesn't — the model has no other way to
// see inside the skill's folder.
func withResourceIndex(skill Skill, content string) string {
	if len(skill.Resources) == 0 {
		return content
	}

	var out strings.Builder
	out.WriteString(content)
	out.WriteString("\n\n---\n\nFiles bundled with this skill, readable with " + ReadSkillToolName + "(name: \"")
	out.WriteString(skill.Name)
	out.WriteString("\", file: ...):\n")
	for _, resource := range skill.Resources {
		out.WriteString("- " + resource + "\n")
	}

	return out.String()
}
