package agents

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

func skillCall(args string) *ToolCall {
	return &ToolCall{FunctionCallMessage: &responses.FunctionCallMessage{Name: ReadSkillToolName, Arguments: args}}
}

func TestSkillSelectionCannotDisableGlobals(t *testing.T) {
	ctx := context.Background()
	var reads []string
	client := stubSkillClient{List: func(context.Context, string, map[string]any) ([]Skill, error) {
		return []Skill{
			{Name: "pdf", Global: true},
			{Name: "notes"},
			{Name: "drafts", Resources: []string{"refs/help.md"}},
			{Name: "help", Resources: []string{"refs/help.md"}},
		}, nil
	}, Read: func(_ context.Context, _ string, _ map[string]any, name, file string) (string, error) {
		reads = append(reads, name+":"+file)
		return name + " content", nil
	}}
	agent := NewAgent(&AgentOptions{Name: "skills", SkillClient: client})

	// Disabling a global has no effect; user skills are on unless disabled.
	in := &AgentInput{Skills: SkillSelection{Disable: []string{"pdf", "drafts", "unknown"}}}
	tools, skills, _, err := agent.prepareSkills(ctx, in, agent.tools)
	require.NoError(t, err)
	require.Len(t, tools, 1)
	require.Equal(t, ReadSkillToolName, functionName(tools[0]))
	var names []string
	for _, s := range skills {
		names = append(names, s.Name)
	}
	require.Equal(t, []string{"pdf", "notes", "help"}, names)

	// The run's enabled listing is the reader's allowlist, resources included.
	for _, name := range names {
		_, err := tools[0].Execute(ctx, skillCall(fmt.Sprintf(`{"name":%q}`, name)))
		require.NoError(t, err)
	}
	for _, args := range []string{`{"name":"drafts"}`, `{"name":"unknown"}`, `{"name":"help","file":"../refs/help.md"}`, `{"name":"help","file":"private.txt"}`} {
		_, err := tools[0].Execute(ctx, skillCall(args))
		require.Error(t, err, args)
	}
	_, err = tools[0].Execute(ctx, skillCall(`{"name":"help","file":"refs/help.md"}`))
	require.NoError(t, err)
	require.Equal(t, []string{"pdf:", "notes:", "help:", "help:refs/help.md"}, reads)

	// The picker catalog shows everything, with globals locked on.
	catalog, err := agent.ListSkills(ctx, "tenant", nil, in.Skills)
	require.NoError(t, err)
	enabled := map[string]bool{}
	for _, listed := range catalog {
		enabled[listed.Name] = listed.Enabled
	}
	require.Equal(t, map[string]bool{"pdf": true, "notes": true, "drafts": false, "help": true}, enabled)
	require.Empty(t, agent.tools) // run reader is not stored on the agent
}

func TestSkillRunIsolation(t *testing.T) {
	client := stubSkillClient{List: func(_ context.Context, namespace string, rc map[string]any) ([]Skill, error) {
		return []Skill{{Name: rc["user"].(string)}}, nil
	}, Read: func(_ context.Context, namespace string, rc map[string]any, name, file string) (string, error) {
		return rc["user"].(string), nil
	}}
	agent := NewAgent(&AgentOptions{Name: "skills", SkillClient: client})
	var wg sync.WaitGroup
	for _, user := range []string{"alice", "bob"} {
		wg.Go(func() {
			in := &AgentInput{RunContext: map[string]any{"user": user}}
			tools, skills, _, err := agent.prepareSkills(context.Background(), in, nil)
			require.NoError(t, err)
			require.Len(t, skills, 1)
			result, err := tools[0].Execute(context.Background(), skillCall(fmt.Sprintf(`{"name":"%s"}`, user)))
			require.NoError(t, err)
			require.Equal(t, user, *result.Output.OfString)
		})
	}
	wg.Wait()
	require.Empty(t, agent.tools)
}

func TestSkillListingValidationAndRefresh(t *testing.T) {
	calls := 0
	agent := NewAgent(&AgentOptions{Name: "skills", SkillClient: stubSkillClient{List: func(context.Context, string, map[string]any) ([]Skill, error) {
		calls++
		return []Skill{{Name: fmt.Sprint(calls), Global: true}}, nil
	}}})
	for i := 1; i <= 2; i++ {
		_, skills, _, err := agent.prepareSkills(context.Background(), &AgentInput{}, nil)
		require.NoError(t, err)
		require.Equal(t, fmt.Sprintf("%d", i), skills[0].Name)
	}

	// The listing is the read allowlist, so ambiguous or unsafe entries fail the run.
	for _, skills := range [][]Skill{
		{{Name: "x/y"}}, {{Name: "x", Resources: []string{"../secret"}}}, {{Name: "x", Global: true}, {Name: "x"}},
	} {
		agent.skillClient = stubSkillClient{List: func(context.Context, string, map[string]any) ([]Skill, error) { return skills, nil }}
		_, _, _, err := agent.prepareSkills(context.Background(), &AgentInput{}, nil)
		require.Error(t, err)
	}
	failure := errors.New("catalog unavailable")
	agent.skillClient = stubSkillClient{List: func(context.Context, string, map[string]any) ([]Skill, error) { return nil, failure }}
	_, _, _, err := agent.prepareSkills(context.Background(), &AgentInput{}, nil)
	require.ErrorIs(t, err, failure)
}

func TestSkillNamespaceIsExplicitForListAndRead(t *testing.T) {
	for _, ns := range []string{"tenant-a", ""} {
		t.Run(ns, func(t *testing.T) {
			rc := map[string]any{"Namespace": "untrusted-legacy-value", "user": "alice"}
			client := stubSkillClient{
				List: func(_ context.Context, namespace string, values map[string]any) ([]Skill, error) {
					require.Equal(t, ns, namespace)
					require.Equal(t, rc, values)
					return []Skill{{Name: "review", Global: true}}, nil
				},
				Read: func(_ context.Context, namespace string, values map[string]any, name, file string) (string, error) {
					require.Equal(t, ns, namespace)
					require.Equal(t, rc, values)
					return namespace + ":" + name, nil
				},
			}
			agent := NewAgent(&AgentOptions{Name: "helper", SkillClient: client})
			tools, _, _, err := agent.prepareSkills(context.Background(), &AgentInput{Namespace: ns, RunContext: rc}, nil)
			require.NoError(t, err)
			result, err := tools[0].Execute(context.Background(), skillCall(`{"name":"review"}`))
			require.NoError(t, err)
			require.Equal(t, ns+":review", *result.Output.OfString)
			require.Equal(t, "untrusted-legacy-value", rc["Namespace"])
		})
	}
}

func TestSkillReaderKeepsItsRunCatalog(t *testing.T) {
	name := "first"
	client := stubSkillClient{List: func(context.Context, string, map[string]any) ([]Skill, error) {
		return []Skill{{Name: name}}, nil
	}, Read: func(_ context.Context, _ string, _ map[string]any, name, file string) (string, error) {
		return name, nil
	}}
	agent := NewAgent(&AgentOptions{Name: "helper", SkillClient: client})
	tools, _, _, err := agent.prepareSkills(context.Background(), &AgentInput{}, nil)
	require.NoError(t, err)
	name = "second"
	current, err := agent.ListSkills(context.Background(), "tenant", nil, SkillSelection{})
	require.NoError(t, err)
	require.Equal(t, "second", current[0].Name)
	_, err = tools[0].Execute(context.Background(), skillCall(`{"name":"second"}`))
	require.Error(t, err)
	result, err := tools[0].Execute(context.Background(), skillCall(`{"name":"first"}`))
	require.NoError(t, err)
	require.Equal(t, "first", *result.Output.OfString)
}
