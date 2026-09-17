package temporal_runtime

import (
	"context"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

func TestDynamicSkillsUseActivities(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	listed, read := 0, 0
	set := agents.SkillSetFuncs{Name: "team", List: func(ctx context.Context, namespace string, rc map[string]any) ([]agents.Skill, error) {
		require.True(t, activity.IsActivity(ctx))
		require.Equal(t, "tenant", namespace)
		require.NotContains(t, rc, "Namespace")
		listed++
		require.Equal(t, "alice", rc["user"])
		return []agents.Skill{{Name: "review", Policy: agents.SkillRequired}}, nil
	}, Resolve: func(ctx context.Context, namespace string, rc map[string]any, name, file string) (string, error) {
		require.True(t, activity.IsActivity(ctx))
		require.Equal(t, "tenant", namespace)
		require.NotContains(t, rc, "Namespace")
		read++
		require.Equal(t, "review", name)
		require.Equal(t, "ref.md", file)
		return "instructions", nil
	}}
	opts := &agents.AgentOptions{Name: "helper", Skills: []agents.SkillSet{set}, History: history.NewConversationManager(history.NewInMemoryConversationPersistence())}
	acts := NewTemporalAgent(nil, opts, nil).GetActivities()
	for _, suffix := range []string{"_ListSkills", "_ResolveSkill", "_ReadSkill"} {
		name := "helper_SkillSet_team" + suffix
		require.Contains(t, acts, name)
		env.RegisterActivityWithOptions(acts[name], activity.RegisterOptions{Name: name})
	}
	env.ExecuteWorkflow(func(ctx workflow.Context) (string, error) {
		ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute})
		proxy := &temporalSkillSet{ctx: ctx, name: "team", prefix: "helper_SkillSet_team"}
		rc := map[string]any{"user": "alice"}
		skills, err := proxy.ListSkills(context.Background(), "tenant", rc)
		if err != nil {
			return "", err
		}
		require.Len(t, skills, 1)
		require.Equal(t, agents.SkillRequired, skills[0].Policy)
		return proxy.ResolveSkillCall(context.Background(), "tenant", rc, "review", "ref.md", &agents.ToolCall{FunctionCallMessage: &responses.FunctionCallMessage{Name: "read_skill", CallID: "c1", Arguments: `{"name":"review","file":"ref.md"}`}})
	})
	require.NoError(t, env.GetWorkflowError())
	var result string
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, "instructions", result)
	require.Equal(t, 1, listed)
	require.Equal(t, 1, read)
}
