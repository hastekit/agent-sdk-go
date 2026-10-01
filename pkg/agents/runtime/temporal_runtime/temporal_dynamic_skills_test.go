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

func TestSkillClientUsesActivities(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	client := &activitySkillClient{t: t}
	opts := &agents.AgentOptions{Name: "helper", SkillClient: client, History: history.NewConversationManager(history.NewInMemoryConversationPersistence())}
	acts := NewTemporalAgent(nil, opts, nil).GetActivities()
	for _, suffix := range []string{"_ListSkills", "_ReadSkill"} {
		name := "helper_Skills" + suffix
		require.Contains(t, acts, name)
		env.RegisterActivityWithOptions(acts[name], activity.RegisterOptions{Name: name})
	}
	env.ExecuteWorkflow(func(ctx workflow.Context) (string, error) {
		ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute})
		proxy := &temporalSkillClientProxy{ctx: ctx, prefix: "helper_Skills"}
		rc := map[string]any{"user": "alice"}
		skills, err := proxy.ListSkills(context.Background(), "tenant", rc)
		if err != nil {
			return "", err
		}
		require.Len(t, skills, 1)
		require.True(t, skills[0].Global)
		_, err = proxy.ReadSkill(context.Background(), "tenant", rc, "review", "")
		require.Error(t, err, "workflow reads must carry their tool call")
		return proxy.ReadSkillCall(context.Background(), "tenant", rc, "review", "ref.md", &agents.ToolCall{FunctionCallMessage: &responses.FunctionCallMessage{Name: "read_skill", CallID: "c1", Arguments: `{"name":"review","file":"ref.md"}`}})
	})
	require.NoError(t, env.GetWorkflowError())
	var result string
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, "instructions", result)
	require.Equal(t, 1, client.listed)
	require.Equal(t, 1, client.read)
}

type activitySkillClient struct {
	t            *testing.T
	listed, read int
}

func (s *activitySkillClient) ListSkills(ctx context.Context, namespace string, rc map[string]any) ([]agents.Skill, error) {
	require.True(s.t, activity.IsActivity(ctx))
	require.Equal(s.t, "tenant", namespace)
	require.NotContains(s.t, rc, "Namespace")
	s.listed++
	require.Equal(s.t, "alice", rc["user"])
	return []agents.Skill{{Name: "review", Global: true}}, nil
}

func (s *activitySkillClient) ReadSkill(ctx context.Context, namespace string, rc map[string]any, name, file string) (string, error) {
	require.True(s.t, activity.IsActivity(ctx))
	require.Equal(s.t, "tenant", namespace)
	require.NotContains(s.t, rc, "Namespace")
	s.read++
	require.Equal(s.t, "review", name)
	require.Equal(s.t, "ref.md", file)
	return "instructions", nil
}
