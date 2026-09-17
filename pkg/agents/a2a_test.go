package agents_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

func TestA2ALocalLoopApprovesAndResumes(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{toolCallResponse("call-1", "release", "{}"), textResponse("released")}}
	tool := newFakeTool("release", true, "done")
	agent := agents.NewAgent(&agents.AgentOptions{Name: "release", Tools: []agents.Tool{tool}}).WithLLM(llm)
	adapter := agent.A2A(&a2a.AgentCard{Name: "release"})
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
	require.Equal(t, a2a.TaskStateInputRequired, task.Status.State)
	require.Zero(t, tool.callCount())
	encoded, err := json.Marshal(llm.request(0))
	require.NoError(t, err)
	require.Contains(t, string(encoded), "ship this release")
	resume := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewDataPart(map[string]any{
		"type": "hastekit.interrupt_response", "resolutions": []any{map[string]any{"call_id": "call-1", "action": "approve"}},
	}))
	resume.TaskID = task.ID
	result, err = client.SendMessage(ctx, &a2a.SendMessageRequest{Message: resume})
	require.NoError(t, err)
	task = result.(*a2a.Task)
	require.Equal(t, a2a.TaskStateCompleted, task.Status.State)
	require.Equal(t, 1, tool.callCount())
	require.Len(t, task.Artifacts, 1)
	require.Equal(t, "released", task.Artifacts[0].Parts[0].Text())
}
