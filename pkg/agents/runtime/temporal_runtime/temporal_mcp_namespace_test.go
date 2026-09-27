package temporal_runtime

import (
	"context"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/internal/testutil"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

type namespaceListing struct {
	namespace string
	context   map[string]any
}

type namespaceMCPToolset struct{ received chan namespaceListing }

func (*namespaceMCPToolset) GetName() string { return "identity" }

func (s *namespaceMCPToolset) ListTools(_ context.Context, namespace string, rc map[string]any) ([]agents.Tool, error) {
	s.received <- namespaceListing{namespace: namespace, context: rc}
	return nil, nil
}

func TestMCPNamespaceCrossesTemporalActivityBoundary(t *testing.T) {
	// Register the real listing activity so the test exercises argument serialization and decoding.
	toolset := &namespaceMCPToolset{received: make(chan namespaceListing, 1)}
	server := NewTemporalMCPClient(testutil.NewMCPClient(toolset), nil)
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivityWithOptions(server.ListTools, activity.RegisterOptions{Name: "identity_ListMCPToolsActivity"})

	// A conflicting free-form value must not replace the explicit workflow argument.
	env.ExecuteWorkflow(func(ctx workflow.Context) error {
		ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute})
		proxy := &temporalMCPClientProxy{ctx: ctx, prefix: "identity"}
		_, _, err := proxy.ListTools(context.Background(), "authenticated-user", map[string]any{"namespace": "spoofed"})
		return err
	})
	require.NoError(t, env.GetWorkflowError())
	select {
	case received := <-toolset.received:
		require.Equal(t, "authenticated-user", received.namespace)
		require.Equal(t, "spoofed", received.context["namespace"])
	default:
		t.Fatal("listing activity did not call the toolset")
	}
}
