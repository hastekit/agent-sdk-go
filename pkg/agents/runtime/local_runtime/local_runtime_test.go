package local_runtime_test

import (
	"context"
	"testing"

	"github.com/hastekit/agent-sdk-go/internal/testutil"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/local_runtime"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func userInput(text string) *agents.AgentInput {
	return &agents.AgentInput{Namespace: "ns", Message: messages.New("user", []responses.InputMessageUnion{{
		OfEasyInput: &responses.EasyMessage{Role: constants.RoleUser, Content: responses.EasyInputContentUnion{OfString: utils.Ptr(text)}},
	}})}
}

func newRuntime(t *testing.T, store *testutil.SpecStore, tools ...agents.Tool) *local_runtime.LocalRuntime {
	t.Helper()
	runtime := local_runtime.NewLocalRuntime(streambroker.NewMemoryStreamBroker())
	factory := store.Factory(history.NewConversationManager(history.NewInMemoryConversationPersistence()), tools...)
	require.NoError(t, runtime.RegisterAgentFactory(factory))
	require.Error(t, runtime.RegisterAgentFactory(factory), "one factory per runtime")
	return runtime
}

func run(t *testing.T, runtime *local_runtime.LocalRuntime, ref agents.AgentRef) (*agents.Agent, string) {
	t.Helper()
	agent, err := runtime.BuildAgent(t.Context(), ref)
	require.NoError(t, err)
	out, err := agent.Run(t.Context(), userInput("hi"))
	require.NoError(t, err)
	return agent, out.Text()
}

// Specs stored after startup run by ref, and a new version applies from the next run.
func TestLocalRuntime_RunsStoredSpecsByRef(t *testing.T) {
	store := testutil.NewSpecStore()
	var sawVersion string
	lookup := testutil.NewFuncTool("lookup", func(_ context.Context, call *agents.ToolCall) (string, error) {
		sawVersion = call.AgentVersion
		return "found", nil
	})
	runtime := newRuntime(t, store, lookup)

	store.Put(testutil.Spec("support", "1", "v1 call:lookup", "lookup"))
	agent, text := run(t, runtime, agents.AgentRef{Name: "support"})
	assert.Equal(t, "v1 call:lookup | found", text)
	assert.Equal(t, "1", agent.Spec().Version)
	assert.Equal(t, "1", sawVersion, "tools see the version they run under")

	store.Put(testutil.Spec("support", "2", "v2 answers"))
	agent, text = run(t, runtime, agents.AgentRef{Name: "support"})
	assert.Equal(t, "v2 answers", text)
	assert.Equal(t, "2", agent.Spec().Version)

	_, err := runtime.BuildAgent(t.Context(), agents.AgentRef{Name: "missing"})
	require.ErrorIs(t, err, agents.ErrAgentNotFound)
	store.Put(testutil.Spec("broken", "1", "x", "deleted_tool"))
	_, err = runtime.BuildAgent(t.Context(), agents.AgentRef{Name: "broken"})
	require.ErrorIs(t, err, agents.ErrInvalidAgentSpec)
}

// Handoff targets load when the model hands off, so specs may point at each other.
func TestLocalRuntime_LoadsHandoffTargetsWhenUsed(t *testing.T) {
	store := testutil.NewSpecStore()
	runtime := newRuntime(t, store)

	root := testutil.Spec("root", "1", "root handoff:billing")
	root.Handoffs = []agents.HandoffSpec{{Name: "billing", Version: "5", Description: "bills"}, {Name: "unused", Description: "never chosen"}}
	billing := testutil.Spec("billing", "5", "billing@5 answers")
	billing.Handoffs = []agents.HandoffSpec{{Name: "root", Version: "1", Description: "hands back"}}
	store.Put(root)
	store.Put(billing)

	_, text := run(t, runtime, agents.AgentRef{Name: "root", Version: "1"})
	assert.Contains(t, text, "billing@5 answers", "the specialist answers after the handoff")
	assert.Equal(t, 1, store.Loads(agents.AgentRef{Name: "billing", Version: "5"}))
	assert.Zero(t, store.Loads(agents.AgentRef{Name: "unused"}), "a target nobody hands off to is never loaded")

	orphan := testutil.Spec("orphan", "1", "handoff:gone")
	orphan.Handoffs = []agents.HandoffSpec{{Name: "gone", Description: "deleted"}}
	store.Put(orphan)
	agent, err := runtime.BuildAgent(t.Context(), agents.AgentRef{Name: "orphan"})
	require.NoError(t, err)
	_, err = agent.Run(t.Context(), userInput("hi"))
	require.ErrorIs(t, err, agents.ErrAgentNotFound)
}
