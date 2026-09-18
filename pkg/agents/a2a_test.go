package agents_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testA2AAuthorizer struct {
	decision chan []responses.InterruptResolution
	canceled chan struct{}
}

func (*testA2AAuthorizer) Instructions(context.Context, *agents.AgentInput, []responses.Interrupt) (string, error) {
	return "Review this action at https://example.com/approvals.", nil
}

func (a *testA2AAuthorizer) Authorize(ctx context.Context, _ *agents.AgentInput, _ []responses.Interrupt) ([]responses.InterruptResolution, error) {
	select {
	case decision := <-a.decision:
		return decision, nil
	case <-ctx.Done():
		close(a.canceled)
		return nil, ctx.Err()
	}
}

func TestA2ALocalLoopRequiresOutOfBandApproval(t *testing.T) {
	for _, action := range []string{"approve", "reject", "cancel", "unconfigured"} {
		t.Run(action, func(t *testing.T) {
			llm := &scriptedLLM{script: []*responses.Response{toolCallResponse("call-1", "release", "{}"), textResponse("finished")}}
			tool := newFakeTool("release", true, "done")
			agent := agents.NewAgent(&agents.AgentOptions{Name: "release", Tools: []agents.Tool{tool}}).WithLLM(llm)
			authorizer := &testA2AAuthorizer{decision: make(chan []responses.InterruptResolution, 1), canceled: make(chan struct{})}
			var options []agents.A2AOption
			if action != "unconfigured" {
				options = append(options, agents.WithA2AAuthorizer(authorizer))
			}
			adapter := agent.A2A(&a2a.AgentCard{Name: "release"}, options...)
			server := httptest.NewServer(adapter.InvokeHandler)
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			client, err := a2aclient.NewFromEndpoints(ctx, []*a2a.AgentInterface{a2a.NewAgentInterface(server.URL, a2a.TransportProtocolJSONRPC)})
			require.NoError(t, err)
			request := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("ship this release"))
			result, err := client.SendMessage(ctx, &a2a.SendMessageRequest{Message: request})
			require.NoError(t, err)
			task := result.(*a2a.Task)
			require.Zero(t, tool.callCount())
			if action == "unconfigured" {
				require.Equal(t, a2a.TaskStateFailed, task.Status.State)
				require.Contains(t, task.Status.Message.Parts[0].Text(), "not configured")
				return
			}
			require.Equal(t, a2a.TaskStateAuthRequired, task.Status.State)
			require.Contains(t, task.Status.Message.Parts[0].Text(), "Approval is required")
			require.Contains(t, task.Status.Message.Parts[1].Text(), "https://example.com/approvals")
			require.Empty(t, task.Metadata)
			if action == "cancel" {
				canceled, err := client.CancelTask(ctx, &a2a.CancelTaskRequest{ID: task.ID})
				require.NoError(t, err)
				require.Equal(t, a2a.TaskStateCanceled, canceled.Status.State)
				select {
				case <-authorizer.canceled:
				case <-ctx.Done():
					t.Fatal("authorization wait was not canceled")
				}
				return
			}
			// The decision arrives from the trusted host, never from an A2A message.
			authorizer.decision <- []responses.InterruptResolution{{CallID: "call-1", Action: action}}
			require.EventuallyWithT(t, func(c *assert.CollectT) {
				finished, err := client.GetTask(ctx, &a2a.GetTaskRequest{ID: task.ID})
				require.NoError(c, err)
				require.Equal(c, a2a.TaskStateCompleted, finished.Status.State)
				require.Len(c, finished.Artifacts, 1)
				require.Equal(c, "finished", finished.Artifacts[0].Parts[0].Text())
			}, 3*time.Second, time.Millisecond)
			if action == "approve" {
				require.Equal(t, 1, tool.callCount())
			} else {
				require.Zero(t, tool.callCount())
			}
		})
	}
}
