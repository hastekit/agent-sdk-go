package tools_test

import (
	"context"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

type answerLLM struct{ text string }

func (l answerLLM) NewStreamingResponses(context.Context, *agents.ModelCall, *responses.Request, func(*responses.ResponseChunk)) (*responses.Response, error) {
	return &responses.Response{Output: []responses.OutputMessageUnion{{OfOutputMessage: &responses.OutputMessage{
		ID: responses.NewOutputItemMessageID(), Role: constants.RoleAssistant,
		Content: &responses.OutputContent{{OfOutputText: &responses.OutputTextContent{Text: l.text}}},
	}}}}, nil
}

func newTestAgent(t *testing.T, name, answer string, store history.ConversationPersistenceAdapter, toolList ...agents.Tool) *agents.Agent {
	t.Helper()
	return agents.NewAgent(&agents.AgentOptions{
		Name:         name,
		History:      history.NewConversationManager(store),
		StreamBroker: streambroker.NewMemoryStreamBroker(),
		Tools:        toolList,
	}).WithLLM(answerLLM{text: answer})
}

func toolCall(name, args, threadID string) *agents.ToolCall {
	return &agents.ToolCall{
		FunctionCallMessage: &responses.FunctionCallMessage{ID: "fc_" + name, CallID: "call_" + name, Name: name, Arguments: args},
		AgentName:           "assistant",
		Namespace:           "tenant",
		ThreadID:            threadID,
	}
}
