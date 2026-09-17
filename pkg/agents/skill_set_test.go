package agents

import (
	"context"
	"errors"
	"fmt"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
	"sync"
	"testing"
)

func TestDynamicSkillsPoliciesAndReader(t *testing.T) {
	ctx := context.Background()
	var reads []string
	set := SkillSetFuncs{Name: "team", List: func(context.Context, string, map[string]any) ([]Skill, error) {
		return []Skill{
			{Name: "required", Policy: SkillRequired}, {Name: "default", Policy: SkillEnabled},
			{Name: "optional", Resources: []string{"refs/help.md"}}, {Name: "blocked", Policy: SkillBlocked},
		}, nil
	}, Resolve: func(_ context.Context, namespace string, _ map[string]any, name, file string) (string, error) {
		reads = append(reads, name+":"+file)
		return name + " content", nil
	}}
	agent := NewAgent(&AgentOptions{Name: "skills", Skills: []SkillSet{testSkillSet(SkillRequired), set}})
	in := &AgentInput{Skills: SkillSelection{Enable: []string{"optional", "blocked"}, Disable: []string{"required", "default"}}}
	tools, skills, _, err := agent.prepareSkills(ctx, in, agent.tools)
	require.NoError(t, err)
	require.Len(t, tools, 1)
	require.Equal(t, ReadSkillToolName, functionName(tools[0]))
	var names []string
	for _, s := range skills {
		names = append(names, s.Name)
	}
	require.Equal(t, []string{"pdf", "required", "optional"}, names)
	for _, name := range []string{"pdf", "required", "optional"} {
		result, err := tools[0].Execute(ctx, skillCall(fmt.Sprintf(`{"name":%q}`, name)))
		require.NoError(t, err)
		require.NotNil(t, result.Output.OfString)
	}
	for _, args := range []string{`{"name":"blocked"}`, `{"name":"default"}`, `{"name":"optional","file":"../refs/help.md"}`, `{"name":"optional","file":"private.txt"}`} {
		_, err := tools[0].Execute(ctx, skillCall(args))
		require.Error(t, err)
	}
	_, err = tools[0].Execute(ctx, skillCall(`{"name":"optional","file":"refs/help.md"}`))
	require.NoError(t, err)
	require.Equal(t, []string{"required:", "optional:", "optional:refs/help.md"}, reads)
	catalog, err := agent.ListSkills(ctx, "default", nil, in.Skills)
	require.NoError(t, err)
	require.Len(t, catalog, 4)
	require.Empty(t, agent.tools) // run reader is not stored on the agent
}

func TestDynamicSkillsRunIsolation(t *testing.T) {
	set := SkillSetFuncs{Name: "tenant", List: func(_ context.Context, namespace string, rc map[string]any) ([]Skill, error) {
		return []Skill{{Name: rc["user"].(string)}}, nil
	}, Resolve: func(_ context.Context, namespace string, rc map[string]any, name, file string) (string, error) {
		return rc["user"].(string), nil
	}}
	agent := NewAgent(&AgentOptions{Name: "skills", Skills: []SkillSet{set}})
	var wg sync.WaitGroup
	for _, user := range []string{"alice", "bob"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in := &AgentInput{RunContext: map[string]any{"user": user}, Skills: SkillSelection{Enable: []string{user}}}
			tools, skills, _, err := agent.prepareSkills(context.Background(), in, nil)
			require.NoError(t, err)
			require.Len(t, skills, 1)
			result, err := tools[0].Execute(context.Background(), skillCall(fmt.Sprintf(`{"name":"%s"}`, user)))
			require.NoError(t, err)
			require.Equal(t, user, *result.Output.OfString)
		}()
	}
	wg.Wait()
	require.Empty(t, agent.tools)
}

func TestDynamicSkillsValidationAndRefresh(t *testing.T) {
	calls := 0
	set := SkillSetFuncs{Name: "team", List: func(context.Context, string, map[string]any) ([]Skill, error) {
		calls++
		return []Skill{{Name: fmt.Sprint(calls), Policy: SkillRequired}}, nil
	}}
	agent := NewAgent(&AgentOptions{Name: "skills", Skills: []SkillSet{set}})
	for i := 1; i <= 2; i++ {
		_, skills, _, err := agent.prepareSkills(context.Background(), &AgentInput{}, nil)
		require.NoError(t, err)
		require.Equal(t, fmt.Sprintf("%d", i), skills[0].Name)
	}
	for _, skills := range [][]Skill{
		{{Name: "x", Policy: "invalid"}}, {{Name: "x/y"}}, {{Name: "x", Resources: []string{"../secret"}}},
	} {
		agent.skillSets = []SkillSet{SkillSetFuncs{Name: "team", List: func(context.Context, string, map[string]any) ([]Skill, error) { return skills, nil }}}
		_, _, _, err := agent.prepareSkills(context.Background(), &AgentInput{}, nil)
		require.Error(t, err)
	}
	failure := errors.New("catalog unavailable")
	agent.skillSets = []SkillSet{SkillSetFuncs{Name: "team", List: func(context.Context, string, map[string]any) ([]Skill, error) { return nil, failure }}}
	_, _, _, err := agent.prepareSkills(context.Background(), &AgentInput{}, nil)
	require.ErrorIs(t, err, failure)
}

func skillCall(args string) *ToolCall {
	return &ToolCall{FunctionCallMessage: &responses.FunctionCallMessage{Name: ReadSkillToolName, Arguments: args}}
}

func TestDynamicSkillCollisionLastSourceWins(t *testing.T) {
	ctx := context.Background()
	for _, policy := range []SkillPolicy{SkillRequired, SkillEnabled, SkillOptIn, SkillBlocked} {
		t.Run(string(policy), func(t *testing.T) {
			source := func(name string, skill Skill) SkillSetFuncs {
				return SkillSetFuncs{Name: name,
					List: func(context.Context, string, map[string]any) ([]Skill, error) { return []Skill{skill}, nil },
					Resolve: func(_ context.Context, namespace string, _ map[string]any, skillName, file string) (string, error) {
						require.Equal(t, "review", skillName)
						return name + ":" + file, nil
					},
				}
			}
			first := source("first", Skill{Name: "review", Policy: SkillRequired, Resources: []string{"old.md"}})
			last := source("last", Skill{Name: "review", Description: "replacement", Policy: policy, Resources: []string{"new.md"}})
			agent := NewAgent(&AgentOptions{Name: "skills", Skills: []SkillSet{first, last}})
			tools, metadata, _, err := agent.prepareSkills(ctx, &AgentInput{}, nil)
			require.NoError(t, err)
			if policy == SkillOptIn || policy == SkillBlocked {
				require.Empty(t, tools)
				require.Empty(t, metadata)
			} else {
				require.Len(t, metadata, 1)
				require.Equal(t, "replacement", metadata[0].Description)
				require.Equal(t, []string{"new.md"}, metadata[0].Resources)
				result, err := tools[0].Execute(ctx, skillCall(`{"name":"review","file":"new.md"}`))
				require.NoError(t, err)
				require.Equal(t, "last:new.md", *result.Output.OfString)
				_, err = tools[0].Execute(ctx, skillCall(`{"name":"review","file":"old.md"}`))
				require.Error(t, err)
			}
			catalog, err := agent.ListSkills(ctx, "default", nil, SkillSelection{Enable: []string{"review"}, Disable: []string{"review"}})
			require.NoError(t, err)
			if policy == SkillBlocked {
				require.Empty(t, catalog)
			} else {
				require.Len(t, catalog, 1)
				require.Equal(t, policy == SkillRequired, catalog[0].Enabled)
			}
		})
	}
}

func TestDynamicSkillCollisionWithinSource(t *testing.T) {
	source := SkillSetFuncs{Name: "source", List: func(context.Context, string, map[string]any) ([]Skill, error) {
		return []Skill{{Name: "review", Policy: SkillRequired}, {Name: "review", Description: "last", Policy: SkillEnabled}}, nil
	}}
	agent := NewAgent(&AgentOptions{Name: "skills", Skills: []SkillSet{source}})
	catalog, err := agent.ListSkills(context.Background(), "default", nil, SkillSelection{})
	require.NoError(t, err)
	require.Len(t, catalog, 1)
	require.Equal(t, "last", catalog[0].Description)
}

func TestSkillNamespaceIsExplicitForListAndRead(t *testing.T) {
	for _, ns := range []string{"tenant-a", ""} {
		t.Run(ns, func(t *testing.T) {
			expected := ns
			if expected == "" {
				expected = "default"
			}
			rc := map[string]any{"Namespace": "untrusted-legacy-value", "user": "alice"}
			set := SkillSetFuncs{Name: "source",
				List: func(_ context.Context, namespace string, values map[string]any) ([]Skill, error) {
					require.Equal(t, expected, namespace)
					require.Equal(t, rc, values)
					return []Skill{{Name: "review", Policy: SkillRequired}}, nil
				},
				Resolve: func(_ context.Context, namespace string, values map[string]any, name, file string) (string, error) {
					require.Equal(t, expected, namespace)
					require.Equal(t, rc, values)
					return namespace + ":" + name, nil
				},
			}
			agent := NewAgent(&AgentOptions{Name: "helper", Skills: []SkillSet{set}})
			tools, _, _, err := agent.prepareSkills(context.Background(), &AgentInput{Namespace: ns, RunContext: rc}, nil)
			require.NoError(t, err)
			result, err := tools[0].Execute(context.Background(), skillCall(`{"name":"review"}`))
			require.NoError(t, err)
			require.Equal(t, expected+":review", *result.Output.OfString)
			require.Equal(t, "untrusted-legacy-value", rc["Namespace"])
		})
	}
}

func TestSkillNamespaceNotInjectedIntoRunContext(t *testing.T) {
	source := SkillSetFuncs{Name: "source", List: func(_ context.Context, ns string, rc map[string]any) ([]Skill, error) {
		require.Equal(t, "tenant", ns)
		require.NotContains(t, rc, "Namespace")
		return nil, nil
	}}
	agent := NewAgent(&AgentOptions{Name: "helper", Skills: []SkillSet{source}})
	_, _, _, err := agent.prepareSkills(context.Background(), &AgentInput{Namespace: "tenant"}, nil)
	require.NoError(t, err)
}

func TestSkillReaderKeepsItsRunCatalog(t *testing.T) {
	name := "first"
	source := SkillSetFuncs{Name: "changing", List: func(context.Context, string, map[string]any) ([]Skill, error) {
		return []Skill{{Name: name, Policy: SkillEnabled}}, nil
	}, Resolve: func(_ context.Context, _ string, _ map[string]any, name, file string) (string, error) {
		return name, nil
	}}
	agent := NewAgent(&AgentOptions{Name: "helper", Skills: []SkillSet{source}})
	tools, _, _, err := agent.prepareSkills(context.Background(), &AgentInput{}, nil)
	require.NoError(t, err)
	name = "second"
	current, err := agent.ListSkills(context.Background(), "default", nil, SkillSelection{})
	require.NoError(t, err)
	require.Equal(t, "second", current[0].Name)
	_, err = tools[0].Execute(context.Background(), skillCall(`{"name":"second"}`))
	require.Error(t, err)
	result, err := tools[0].Execute(context.Background(), skillCall(`{"name":"first"}`))
	require.NoError(t, err)
	require.Equal(t, "first", *result.Output.OfString)
}
