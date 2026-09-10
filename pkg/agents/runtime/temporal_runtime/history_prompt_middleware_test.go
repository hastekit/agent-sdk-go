package temporal_runtime_test

import (
	"context"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/temporal_runtime"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
)

type activityPersistenceMiddleware struct {
	agents.NoopMiddleware
	t *testing.T
}

func (m activityPersistenceMiddleware) WrapLoadMessages(next agents.LoadMessagesFunc) agents.LoadMessagesFunc {
	return func(ctx context.Context, r *agents.LoadMessagesRequest) ([]history.ConversationMessage, error) {
		require.True(m.t, activity.IsActivity(ctx))
		rows, err := next(ctx, r)
		return append(rows, history.ConversationMessage{ThreadID: "added inside activity"}), err
	}
}

func (m activityPersistenceMiddleware) WrapSaveMessages(next agents.SaveMessagesFunc) agents.SaveMessagesFunc {
	return func(ctx context.Context, r *agents.SaveMessagesRequest) error {
		require.True(m.t, activity.IsActivity(ctx))
		copy := *r
		copy.Meta = map[string]any{"source": "middleware"}
		return next(ctx, &copy)
	}
}

func (m activityPersistenceMiddleware) WrapGetPrompt(agents.GetPromptFunc) agents.GetPromptFunc {
	return func(ctx context.Context, _ *agents.Dependencies) (string, error) {
		require.True(m.t, activity.IsActivity(ctx))
		return "cached prompt inside activity", nil
	}
}

func TestTemporalHistoryAndPromptMiddlewareUseExistingActivities(t *testing.T) {
	store := history.NewInMemoryConversationPersistence()
	a := temporal_runtime.NewTemporalAgent(nil, &agents.AgentOptions{
		Name: "agent", History: history.NewConversationManager(store),
		Middlewares: []agents.Middleware{activityPersistenceMiddleware{t: t}},
	}, nil)
	activities := a.GetActivities()
	for name := range activities {
		require.NotContains(t, name, "Middleware")
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	for _, name := range []string{"agent_LoadMessagesActivity", "agent_SaveMessagesActivity", "agent_GetPromptActivity"} {
		env.RegisterActivityWithOptions(activities[name], activity.RegisterOptions{Name: name})
	}
	_, err := env.ExecuteActivity("agent_SaveMessagesActivity", "tenant", "run", "", "thread", "conversation", []history.Message{}, map[string]any{"source": "original"})
	require.NoError(t, err)
	value, err := env.ExecuteActivity("agent_LoadMessagesActivity", "tenant", "thread", "")
	require.NoError(t, err)
	var rows []history.ConversationMessage
	require.NoError(t, value.Get(&rows))
	require.Len(t, rows, 2)
	require.Equal(t, "middleware", rows[0].Meta["source"])
	require.Equal(t, "added inside activity", rows[1].ThreadID, "the activity journals the transformed result")
	value, err = env.ExecuteActivity("agent_GetPromptActivity", &agents.Dependencies{})
	require.NoError(t, err)
	var prompt string
	require.NoError(t, value.Get(&prompt))
	require.Equal(t, "cached prompt inside activity", prompt)
}
