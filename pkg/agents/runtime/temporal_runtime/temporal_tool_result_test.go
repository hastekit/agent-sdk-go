package temporal_runtime

import (
	"context"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// A client tool waits for its result through the broker proxy, which runs the
// wait as an activity instead of in workflow code.
func TestWaitToolResultRunsAsAnActivity(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()
	historyManager := history.NewConversationManager(history.NewInMemoryConversationPersistence())
	acts := NewTemporalAgent(nil, &agents.AgentOptions{Name: "helper", History: historyManager}, broker).GetActivities()
	name := "helper_WaitToolResultActivity"
	require.Contains(t, acts, name, "client tools need no configuration")

	// The client answers while the run is live: the output is kept for the call.
	output := "selected text"
	require.NoError(t, broker.EnqueueMessage(context.Background(), "stream-1", history.Message{Messages: []responses.InputMessageUnion{{
		OfFunctionCallOutput: &responses.FunctionCallOutputMessage{CallID: "call_sel", Output: responses.FunctionCallOutputContentUnion{OfString: &output}},
	}}}))

	for _, tc := range []struct {
		callID string
		want   toolResultWait
	}{
		{"call_sel", toolResultWait{Result: "selected text", Found: true}},
		{"call_none", toolResultWait{}},
	} {
		var suite testsuite.WorkflowTestSuite
		env := suite.NewTestWorkflowEnvironment()
		env.RegisterActivityWithOptions(acts[name], activity.RegisterOptions{Name: name})
		env.ExecuteWorkflow(func(ctx workflow.Context) (toolResultWait, error) {
			ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute})
			proxy := NewTemporalStreamBrokerProxy(ctx, "helper", broker)
			result, found, err := proxy.WaitToolResult(context.Background(), "stream-1", tc.callID, 50*time.Millisecond)
			return toolResultWait{Result: result, Found: found}, err
		})
		require.NoError(t, env.GetWorkflowError())
		var got toolResultWait
		require.NoError(t, env.GetWorkflowResult(&got))
		require.Equal(t, tc.want, got, tc.callID)
	}
}
