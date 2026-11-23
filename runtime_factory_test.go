package sdk

import (
	"context"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/internal/testutil"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/temporal_runtime"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func factoryUserInput(text string) *agents.AgentInput {
	return &agents.AgentInput{Namespace: "ns", Message: messages.New("user", []responses.InputMessageUnion{{
		OfEasyInput: &responses.EasyMessage{Role: constants.RoleUser, Content: responses.EasyInputContentUnion{OfString: utils.Ptr(text)}},
	}})}
}

// whoamiFactory serves who@1, whose tool echoes the amg_id gateway value.
func whoamiFactory() (*testutil.MapFactory, agents.AgentRef) {
	persistence := history.NewInMemoryConversationPersistence()
	store := testutil.NewMapFactory()
	ref := agents.AgentRef{Name: "who", Version: "1"}
	store.Put(ref, func() *agents.AgentOptions {
		return (&AgentConfig{
			Name: "who", LLM: testutil.EchoProvider{}, Instruction: NewPrompt("call:whoami"),
			Tools:   []Tool{testutil.NewGatewayContextTool("whoami", "amg_id")},
			History: history.NewConversationManager(persistence),
		}).Options()
	})
	return store, ref
}

func TestTemporalRuntime_RegistersOneFactoryBeforeServing(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()
	rt := &TemporalRuntime{TemporalRuntime: temporal_runtime.NewTemporalRuntime(nil, broker), broker: broker}
	store, ref := whoamiFactory()

	_, err := rt.BuildAgent(t.Context(), ref)
	require.Error(t, err, "no factory yet")
	require.Error(t, rt.RegisterAgentFactory(nil))
	require.NoError(t, rt.RegisterAgentFactory(store))
	require.Error(t, rt.RegisterAgentFactory(store), "one factory per runtime")

	agent, err := rt.BuildAgent(t.Context(), ref)
	require.NoError(t, err)
	assert.True(t, agent.FactoryBuilt())
	assert.Equal(t, ref, agent.Ref())
	assert.Same(t, broker, agent.StreamBroker())

	// A factory alone is enough to serve.
	_, _, finish, err := rt.lifecycle.begin(t.Context())
	require.NoError(t, err)
	assert.NotNil(t, rt.lifecycle.servingFactory())
	require.Error(t, rt.RegisterAgentFactory(store), "registration closes once serving starts")
	finish()
}

// Through the public API: the worker serves a static agent and factory agents side by side,
// and the gateway context set on the caller reaches the factory agent's tool.
func TestTemporalRuntime_ServesFactoryAgentsAlongsideStaticOnes(t *testing.T) {
	hostPort := testutil.StartTemporalDevServer(t)
	broker := streambroker.NewMemoryStreamBroker()
	rt, err := NewTemporalRuntime(hostPort, broker)
	require.NoError(t, err)
	defer rt.Close()

	static, err := NewAgent(&AgentConfig{Name: "static", LLM: testutil.EchoProvider{}, Instruction: NewPrompt("static agent")}, WithRuntime(rt))
	require.NoError(t, err)
	store, ref := whoamiFactory()
	require.NoError(t, rt.RegisterAgentFactory(store))

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	serveCtx, stop := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- rt.Serve(serveCtx) }()
	defer func() { stop(); <-served }()

	agent, err := rt.BuildAgent(ctx, ref)
	require.NoError(t, err)
	out, err := agent.Run(gateway.AddContext(ctx, map[string]string{"amg_id": "amg-42"}), factoryUserInput("who"))
	require.NoError(t, err)
	assert.Equal(t, "call:whoami | amg_id=amg-42", out.Text())

	out, err = static.Run(ctx, factoryUserInput("hi"))
	require.NoError(t, err)
	assert.Equal(t, "static agent", out.Text(), "registered agents keep their own workflow")
}
