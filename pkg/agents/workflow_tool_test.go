package agents_test

import (
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/workflow"
	"github.com/stretchr/testify/require"
)

func TestAgentInvokesWorkflowAndResumesHumanNode(t *testing.T) {
	for _, action := range []string{"approve", "reject"} {
		t.Run(action, func(t *testing.T) {
			compiled, err := workflow.LoadYAML([]byte(`version: 1
id: review
nodes:
 - {id: human, type: human, config: {message: "Approve this workflow?"}}
 - {id: approved, type: javascript, config: {code: 'return "approved-workflow";'}}
 - {id: rejected, type: javascript, config: {code: 'return "rejected-workflow";'}}
edges:
 - {from: human, port: approved, to: approved}
 - {from: human, port: rejected, to: rejected}
`), workflow.Dependencies{})
			require.NoError(t, err)
			tool, err := workflow.NewTool("review", "Review a request", nil, compiled)
			require.NoError(t, err)
			model := &scriptedLLM{script: []*responses.Response{toolCallResponse("workflow-call", "review", "{}"), textResponse("done")}}
			agent := newScriptedAgent("main", model, nil, nil, []agents.Tool{tool}, nil)
			out := runAgent(t, agent, &agents.AgentInput{Namespace: "test", ThreadID: "workflow-" + action, Message: userMessage("run review")})
			requireStatus(t, out, agentstate.RunStatusPaused)
			require.Len(t, out.Interrupts, 1)
			id := out.Interrupts[0].FunctionCallMessage.CallID
			require.NotEqual(t, "workflow-call", id)
			approved, rejected := []string{}, []string{}
			if action == "approve" {
				approved = append(approved, id)
			} else {
				rejected = append(rejected, id)
			}
			out = runAgent(t, agent, &agents.AgentInput{Namespace: "test", ThreadID: "workflow-" + action, PreviousRunID: out.RunID, Message: approvalMessage(approved, rejected)})
			requireStatus(t, out, agentstate.RunStatusCompleted)
			word := "approved-workflow"
			if action == "reject" {
				word = "rejected-workflow"
			}
			require.Contains(t, messagesText(out.Output), word)
		})
	}
}
