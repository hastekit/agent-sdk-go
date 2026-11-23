package local_runtime_test

import (
	"testing"

	"github.com/hastekit/agent-sdk-go/internal/testutil"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/agents/prompts"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/local_runtime"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func agentConfig(name, prompt string, tools []agents.Tool, handoffs ...*agents.Handoff) func() *agents.AgentOptions {
	persistence := history.NewInMemoryConversationPersistence()
	return func() *agents.AgentOptions {
		return &agents.AgentOptions{
			Name:        name,
			LLM:         testutil.EchoProvider{},
			Instruction: prompts.New(prompt),
			History:     history.NewConversationManager(persistence),
			Tools:       tools,
			Handoffs:    handoffs,
		}
	}
}

func userInput(text string) *agents.AgentInput {
	return &agents.AgentInput{Namespace: "ns", Message: messages.New("user", []responses.InputMessageUnion{{
		OfEasyInput: &responses.EasyMessage{Role: constants.RoleUser, Content: responses.EasyInputContentUnion{OfString: utils.Ptr(text)}},
	}})}
}

// The same factory runs on the local runtime: configs added or versioned after
// startup are picked up by ref, and handoff targets resolve through the factory.
func TestLocalRuntime_RunsFactoryAgentsByRef(t *testing.T) {
	store := testutil.NewMapFactory()
	runtime := local_runtime.NewLocalRuntime(streambroker.NewMemoryStreamBroker())
	require.NoError(t, runtime.RegisterAgentFactory(store))
	require.Error(t, runtime.RegisterAgentFactory(store), "one factory per runtime")

	v1 := agents.AgentRef{Name: "an3", Version: "1"}
	store.Put(v1, agentConfig("an3", "v1 call:lookup", []agents.Tool{testutil.NewTextTool("lookup", "found-v1")}))
	agent, err := runtime.BuildAgent(t.Context(), v1)
	require.NoError(t, err)
	out, err := agent.Run(t.Context(), userInput("hi"))
	require.NoError(t, err)
	assert.Equal(t, "v1 call:lookup | found-v1", out.Text())

	v2 := agents.AgentRef{Name: "an3", Version: "2"}
	store.Put(v2, agentConfig("an3", "v2 handoff:billing", nil, &agents.Handoff{Name: "billing", Description: "bills", Version: "5"}))
	store.Put(agents.AgentRef{Name: "billing", Version: "5"}, agentConfig("billing", "billing@5 answers", nil))
	agent, err = runtime.BuildAgent(t.Context(), v2)
	require.NoError(t, err)
	out, err = agent.Run(t.Context(), userInput("hi"))
	require.NoError(t, err)
	assert.Contains(t, out.Text(), "billing@5 answers", "the specialist answers after the handoff")

	_, err = runtime.BuildAgent(t.Context(), agents.AgentRef{Name: "missing"})
	require.ErrorIs(t, err, agents.ErrAgentNotFound)
}
