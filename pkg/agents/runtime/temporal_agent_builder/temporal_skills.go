package temporal_agent_builder

import (
	"context"
	"errors"
	"fmt"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentconfig"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/temporal_runtime"
	"go.temporal.io/sdk/workflow"
)

const (
	listSkillsActivity = WorkflowName + "_ListSkillsActivity"
	readSkillActivity  = WorkflowName + "_ReadSkillActivity"
)

// SkillListInput is the skill listing activity's input: the config's skill
// picks and the run's namespace and context.
type SkillListInput struct {
	Skills     []string       `json:"skills"`
	Namespace  string         `json:"namespace"`
	RunContext map[string]any `json:"run_context,omitempty"`
}

// SkillReadInput is the read_skill activity's input: the picks, which skill
// and file to read, and the tool call it answers.
type SkillReadInput struct {
	Skills     []string         `json:"skills"`
	Namespace  string           `json:"namespace"`
	RunContext map[string]any   `json:"run_context,omitempty"`
	Name       string           `json:"name"`
	File       string           `json:"file,omitempty"`
	Call       *agents.ToolCall `json:"call"`
}

// skillClient rebuilds the config's skill client: the Builder's stores
// filtered to the picks.
func (a *AgentBuilder) skillClient(names []string) (*temporal_runtime.TemporalSkillClient, error) {
	client := a.builder.SkillClient(names)
	if client == nil {
		return nil, configError(fmt.Errorf("%w: the worker has no skill stores for this agent's skills", agentconfig.ErrInvalid))
	}
	return temporal_runtime.NewTemporalSkillClient(client, a.broker, agents.ToolCallMiddlewaresOf(a.builder.Middlewares())...), nil
}

// ListSkills lists the picked skills, global and the namespace's own.
func (a *AgentBuilder) ListSkills(ctx context.Context, in *SkillListInput) ([]agents.Skill, error) {
	client, err := a.skillClient(in.Skills)
	if err != nil {
		return nil, err
	}
	return client.ListSkills(ctx, in.Namespace, in.RunContext)
}

// ReadSkill reads a picked skill for read_skill; a skill left out of the picks
// is not found.
func (a *AgentBuilder) ReadSkill(ctx context.Context, in *SkillReadInput) (string, error) {
	client, err := a.skillClient(in.Skills)
	if err != nil {
		return "", err
	}
	return client.ReadSkill(ctx, in.Namespace, in.RunContext, in.Name, in.File, in.Call)
}

// skillClientProxy is the workflow-side skill client: listing and reading run
// in activities carrying the config's skill picks.
type skillClientProxy struct {
	ctx    workflow.Context
	skills []string
}

var (
	_ agents.SkillClient       = (*skillClientProxy)(nil)
	_ agents.SkillReadExecutor = (*skillClientProxy)(nil)
)

func newSkillClientProxy(ctx workflow.Context, skills []string) agents.SkillClient {
	return &skillClientProxy{ctx: ctx, skills: skills}
}

func (s *skillClientProxy) ListSkills(_ context.Context, namespace string, runContext map[string]any) ([]agents.Skill, error) {
	var skills []agents.Skill
	err := workflow.ExecuteActivity(s.ctx, listSkillsActivity, &SkillListInput{Skills: s.skills, Namespace: namespace, RunContext: runContext}).Get(s.ctx, &skills)
	return skills, err
}

// ReadSkill is not used by the workflow: read_skill carries its call through
// ReadSkillCall.
func (s *skillClientProxy) ReadSkill(context.Context, string, map[string]any, string, string) (string, error) {
	return "", errors.New("direct skill reads are not supported by the workflow proxy; use read_skill")
}

func (s *skillClientProxy) ReadSkillCall(_ context.Context, namespace string, runContext map[string]any, name, file string, call *agents.ToolCall) (string, error) {
	var content string
	input := &SkillReadInput{Skills: s.skills, Namespace: namespace, RunContext: runContext, Name: name, File: file, Call: call}
	err := workflow.ExecuteActivity(s.ctx, readSkillActivity, input).Get(s.ctx, &content)
	return content, err
}
