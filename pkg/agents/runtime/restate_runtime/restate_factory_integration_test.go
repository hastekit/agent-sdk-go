package restate_runtime_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/internal/testutil"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/agents/prompts"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/restate_runtime"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/restatedev/sdk-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveFactory runs one service process on port until the returned stop is called.
func serveFactory(t *testing.T, port string, factory agents.AgentFactory, broker agents.StreamBroker) func() {
	t.Helper()
	handler, err := restate_runtime.BindFactory(server.NewRestate(), factory, broker).Handler()
	require.NoError(t, err)
	var listener net.Listener
	for range 50 {
		if listener, err = net.Listen("tcp", "127.0.0.1:"+port); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.NoError(t, err)
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: handler, Protocols: &protocols, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Log(err)
		}
	}()
	var once sync.Once
	stop := func() { once.Do(func() { _ = srv.Close() }) }
	t.Cleanup(stop)
	return stop
}

func input(text string) *agents.AgentInput {
	return &agents.AgentInput{Namespace: "ns", ThreadID: "thread-" + text, Message: messages.New("user", []responses.InputMessageUnion{{
		OfEasyInput: &responses.EasyMessage{Role: constants.RoleUser, Content: responses.EasyInputContentUnion{OfString: utils.Ptr(text)}},
	}})}
}

func TestFactoryWorkflow_RunsOnRestate(t *testing.T) {
	r := testutil.StartRestate(t)
	broker := streambroker.NewMemoryStreamBroker()
	persistence := history.NewInMemoryConversationPersistence()
	config := func(name, prompt string, tools []agents.Tool, handoffs ...*agents.Handoff) func() *agents.AgentOptions {
		return func() *agents.AgentOptions {
			return &agents.AgentOptions{Name: name, LLM: testutil.EchoProvider{}, Instruction: prompts.New(prompt),
				History: history.NewConversationManager(persistence), Tools: tools, Handoffs: handoffs}
		}
	}
	rt := restate_runtime.NewRestateRuntime(r.IngressURL, broker)
	run := func(factory agents.AgentFactory, ctx context.Context, ref agents.AgentRef, in *agents.AgentInput) (*agents.AgentOutput, error) {
		agent, err := agents.NewFactoryAgent(ctx, factory, ref, func(o *agents.AgentOptions) { o.Runtime = rt; o.StreamBroker = broker })
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		return agent.Run(ctx, in)
	}

	// The service is registered before any agent exists.
	port := testutil.FreePort(t)
	first := testutil.NewMapFactory()
	stopFirst := serveFactory(t, port, first, broker)
	r.Register(t, port)

	t.Run("an agent added after startup runs, and a new version applies from the next run", func(t *testing.T) {
		v1 := agents.AgentRef{Name: "an3", Version: "1"}
		first.Put(v1, config("an3", "v1 call:lookup", []agents.Tool{testutil.NewTextTool("lookup", "found-v1")}))
		out, err := run(first, t.Context(), v1, input("a"))
		require.NoError(t, err)
		assert.Equal(t, "v1 call:lookup | found-v1", out.Text())

		v2 := agents.AgentRef{Name: "an3", Version: "2"}
		first.Put(v2, config("an3", "v2 call:search", []agents.Tool{testutil.NewTextTool("search", "found-v2")}))
		out, err = run(first, t.Context(), v2, input("b"))
		require.NoError(t, err)
		assert.Equal(t, "v2 call:search | found-v2", out.Text())
	})

	t.Run("handoffs between factory agents", func(t *testing.T) {
		root := agents.AgentRef{Name: "root", Version: "4"}
		first.Put(root, config("root", "root handoff:billing", nil, &agents.Handoff{Name: "billing", Description: "bills", Version: "9"}))
		first.Put(agents.AgentRef{Name: "billing", Version: "9"}, config("billing", "billing@9 call:invoice",
			[]agents.Tool{testutil.NewTextTool("invoice", "invoice-77")}, &agents.Handoff{Name: "root", Description: "back", Version: "4"}))
		out, err := run(first, t.Context(), root, input("c"))
		require.NoError(t, err)
		assert.Equal(t, "billing@9 call:invoice | invoice-77", out.Text())
	})

	t.Run("gateway context reaches the steps", func(t *testing.T) {
		ref := agents.AgentRef{Name: "who", Version: "1"}
		first.Put(ref, config("who", "call:whoami", []agents.Tool{testutil.NewGatewayContextTool("whoami", "amg_id")}))
		ctx := gateway.AddContext(t.Context(), map[string]string{"amg_id": "amg-42"})
		out, err := run(first, ctx, ref, input("d"))
		require.NoError(t, err)
		assert.Equal(t, "call:whoami | amg_id=amg-42", out.Text())
	})

	t.Run("an in-flight run finishes on its version after a restart", func(t *testing.T) {
		started := make(chan struct{})
		var once sync.Once
		slow := agents.AgentRef{Name: "slow", Version: "1"}
		first.Put(slow, config("slow", "slow@1 call:wait", []agents.Tool{testutil.NewFuncTool("wait", func(ctx context.Context, _ *agents.ToolCall) (string, error) {
			once.Do(func() { close(started) })
			<-ctx.Done()
			return "", ctx.Err()
		})}))
		done := make(chan *agents.AgentOutput, 1)
		go func() {
			out, err := run(first, context.Background(), slow, input("e"))
			assert.NoError(t, err)
			done <- out
		}()
		select {
		case <-started:
		case <-time.After(30 * time.Second):
			t.Fatal("the tool never started")
		}

		// A new process on the same address, whose store has moved on to slow@2.
		stopFirst()
		second := testutil.NewMapFactory()
		second.Put(slow, config("slow", "slow@1 call:wait", []agents.Tool{testutil.NewTextTool("wait", "waited")}))
		second.Put(agents.AgentRef{Name: "slow", Version: "2"}, config("slow", "slow@2 call:other", []agents.Tool{testutil.NewTextTool("other", "nope")}))
		serveFactory(t, port, second, broker)

		select {
		case out := <-done:
			assert.Equal(t, "slow@1 call:wait | waited", out.Text(), "the run finishes on the version it started with")
		case <-time.After(90 * time.Second):
			t.Fatal("the run did not finish after the restart")
		}
		assert.Zero(t, second.Builds(agents.AgentRef{Name: "slow", Version: "2"}))
	})
}
