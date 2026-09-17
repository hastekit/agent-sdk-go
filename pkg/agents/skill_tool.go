package agents

import (
	"context"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

// ReadSkillToolName is the name the model calls to read a skill. The prompt
// names the same tool, so the two never drift apart.
const ReadSkillToolName = "read_skill"

type ReadSkillInput struct {
	Name string `json:"name"`
	File string `json:"file,omitempty"`
}

func dynamicSkillDescriptor() *BaseTool {
	return &BaseTool{
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
		// Sources are read-only but may resolve content remotely.
		Annotations: &ToolAnnotations{
			Title:           "Read skill",
			ReadOnlyHint:    utils.Ptr(true),
			DestructiveHint: utils.Ptr(false),
			IdempotentHint:  utils.Ptr(true),
			OpenWorldHint:   utils.Ptr(true),
		},
	}
}

func skillResponse(params *ToolCall, output string) *ToolCallResponse {
	return &ToolCallResponse{
		FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			ID:     params.ID,
			CallID: params.CallID,
			Output: responses.FunctionCallOutputContentUnion{
				OfString: utils.Ptr(output),
			},
		},
	}
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

type resolvedSkillTool struct {
	*BaseTool
	bindings   map[string]skillBinding
	namespace  string
	runContext map[string]any
}

func (t *resolvedSkillTool) Execute(ctx context.Context, call *ToolCall) (*ToolCallResponse, error) {
	var in ReadSkillInput
	if err := sonic.Unmarshal([]byte(call.Arguments), &in); err != nil {
		return nil, err
	}
	binding, ok := t.bindings[in.Name]
	if !ok {
		return nil, fmt.Errorf("skill %q is not enabled for this run", in.Name)
	}
	if in.File != "" && (!fs.ValidPath(in.File) || strings.Contains(in.File, "\\") || (in.File != SkillFileName && !slices.Contains(binding.skill.Resources, in.File))) {
		return nil, fmt.Errorf("skill %q has no allowed file %q", in.Name, in.File)
	}
	var content string
	var err error
	if executor, ok := binding.set.(SkillReadExecutor); ok {
		content, err = executor.ResolveSkillCall(ctx, t.namespace, t.runContext, binding.originalName, in.File, call)
	} else {
		content, err = binding.set.ResolveSkill(ctx, t.namespace, t.runContext, binding.originalName, in.File)
	}
	if err != nil {
		return nil, err
	}
	if in.File == "" {
		content = withResourceIndex(binding.skill, content)
	}
	return skillResponse(call, content), nil
}

// SkillReadExecutor is implemented by durable runtime proxies to carry the tool
// call to their execution boundary for tracing, stop handling, and middleware.
// Application SkillSets need only implement ResolveSkill.
type SkillReadExecutor interface {
	ResolveSkillCall(context.Context, string, map[string]any, string, string, *ToolCall) (string, error)
}

// ResolveSkillWithMiddleware runs a resolver inside a durable execution step.
// Local tools already receive this wrapping from their ToolExecutor.
func ResolveSkillWithMiddleware(ctx context.Context, set SkillSet, namespace string, rc map[string]any, name, file string, call *ToolCall, middlewares []ToolCallMiddleware) (string, error) {
	descriptor := dynamicSkillDescriptor()
	resp, err := ExecuteWithTrace(ctx, nil, call, func(ctx context.Context, call *ToolCall) (*ToolCallResponse, error) {
		return ExecuteToolCallWithMiddleware(ctx, middlewares, SerializeTool(descriptor, call), call, func(ctx context.Context, call *ToolCall) (*ToolCallResponse, error) {
			content, err := set.ResolveSkill(ctx, namespace, rc, name, file)
			if err != nil {
				return nil, err
			}
			return skillResponse(call, content), nil
		})
	})
	if err != nil {
		return "", err
	}
	if resp == nil || resp.FunctionCallOutputMessage == nil || resp.Output.OfString == nil {
		return "", fmt.Errorf("skill reader middleware returned no text")
	}
	return *resp.Output.OfString, nil
}

// ValidateSkillSets checks static names without listing remote content. Call it
// before registering configurations with a durable runtime.
func ValidateSkillSets(sets []SkillSet) error {
	seen := map[string]bool{}
	for _, set := range sets {
		if set == nil || !validSkillPart(set.GetName()) {
			return fmt.Errorf("skill set must have a nonempty name without slashes")
		}
		if seen[set.GetName()] {
			return fmt.Errorf("duplicate skill set %q", set.GetName())
		}
		seen[set.GetName()] = true
	}
	return nil
}
