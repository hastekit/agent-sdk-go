package sdk

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/internal/testutil"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentconfig"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startRestateBuilder starts a Restate server in Docker and serves a fixture's
// Builder through it. Set RESTATE_IT=1 to run the tests that use it;
// RESTATE_IMAGE overrides the image.
func startRestateBuilder(t *testing.T, steps ...testutil.Step) (*testutil.AgentFixture, *RestateRuntime, context.Context) {
	t.Helper()
	if os.Getenv("RESTATE_IT") != "1" {
		t.Skip("set RESTATE_IT=1 to run against a Restate server in Docker")
	}
	image := os.Getenv("RESTATE_IMAGE")
	if image == "" {
		image = "docker.restate.dev/restatedev/restate:latest"
	}
	ingressPort, adminPort, servicePort := freePort(t), freePort(t), freePort(t)
	out, err := exec.Command("docker", "run", "-d", "--rm",
		"-p", fmt.Sprintf("%d:8080", ingressPort), "-p", fmt.Sprintf("%d:9070", adminPort),
		"--add-host=host.docker.internal:host-gateway", image).CombinedOutput()
	require.NoError(t, err, string(out))
	container := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", container).Run() })
	admin := fmt.Sprintf("http://127.0.0.1:%d", adminPort)
	eventually(t, 90*time.Second, func() error {
		resp, err := http.Get(admin + "/health")
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("admin health %d", resp.StatusCode)
		}
		return nil
	})

	broker := streambroker.NewMemoryStreamBroker()
	runtime, err := NewRestateRuntime(fmt.Sprintf("http://127.0.0.1:%d", ingressPort), broker)
	require.NoError(t, err)
	f := testutil.NewAgentFixture(t, runtime, nil, steps...)
	require.NoError(t, runtime.RegisterBuilder(f.Builder))

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- runtime.Serve(ctx, fmt.Sprintf("0.0.0.0:%d", servicePort)) }()
	t.Cleanup(func() { cancel(); <-served; _ = runtime.Close() })

	eventually(t, 60*time.Second, func() error {
		body := fmt.Sprintf(`{"uri":"http://host.docker.internal:%d","force":true}`, servicePort)
		resp, err := http.Post(admin+"/deployments", "application/json", bytes.NewBufferString(body))
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("register deployment: %d", resp.StatusCode)
		}
		return nil
	})
	return f, runtime, ctx
}

// TestRestateRunsAConfigAddedAfterServeStarts runs a config-built agent
// through a real Restate server.
func TestRestateRunsAConfigAddedAfterServeStarts(t *testing.T) {
	f, runtime, ctx := startRestateBuilder(t, testutil.FixtureSteps()...)

	// Written after Serve started: nothing about it was registered.
	f.AddConfig(t)
	agent, err := f.Agent(ctx, testutil.FixtureNamespace, testutil.FixtureAgent)
	require.NoError(t, err)
	runCtx, stop := context.WithTimeout(ctx, 60*time.Second)
	defer stop()
	result, err := agent.Run(runCtx, &agents.AgentInput{
		Namespace: testutil.FixtureNamespace,
		ThreadID:  "thread-1",
		Message:   history.Message{Messages: []responses.InputMessageUnion{responses.UserMessage("go")}},
	})
	require.NoError(t, err)
	require.Equal(t, agentstate.RunStatusCompleted, result.Status)

	seen := f.Provider.Seen()
	require.Len(t, seen, 4)
	assert.Equal(t, []string{testutil.FixturePickedMCP, testutil.FixturePickedTool, agents.ReadSkillToolName}, seen[0].Tools)
	assert.NotContains(t, seen[0].Instructions, testutil.FixtureOtherSkill)
	assert.Equal(t, 1, f.Calls.Of(testutil.FixturePickedTool))
	assert.Equal(t, 1, f.Calls.Of(testutil.FixturePickedMCP))
	assert.Zero(t, f.Calls.Of(testutil.FixtureOtherTool))

	// Another namespace can neither find the agent nor run its snapshot.
	_, err = f.Agent(ctx, "tenant-b", testutil.FixtureAgent)
	require.ErrorIs(t, err, agentconfig.ErrNotFound)
	snapshot, err := f.Registry.Snapshot(ctx, testutil.FixtureNamespace, testutil.FixtureAgent)
	require.NoError(t, err)
	_, err = runtime.Bind(snapshot).Run(runCtx, nil, &agents.AgentInput{Namespace: "tenant-b", ThreadID: "thread-2"})
	require.Error(t, err)
	require.Len(t, f.Provider.Seen(), 4, "the refused run never reached the model")
}

// TestRestateDeliversAConfigBuiltAgentsBackgroundTask starts a background
// task from a config-built agent on a real Restate server: the background
// service waits on the catalog tool and its result starts a new run.
func TestRestateDeliversAConfigBuiltAgentsBackgroundTask(t *testing.T) {
	f, _, ctx := startRestateBuilder(t,
		testutil.Step{Tool: testutil.FixtureBackgroundTool},
		testutil.Step{Text: "indexing has started"},
		testutil.Step{Text: "indexing finished"},
	)
	_, err := f.Store.Put(ctx, testutil.FixtureNamespace, agentconfig.Config{Name: "indexer", Model: testutil.FixtureModel, Tools: []string{testutil.FixtureBackgroundTool}})
	require.NoError(t, err)
	agent, err := f.Agent(ctx, testutil.FixtureNamespace, "indexer")
	require.NoError(t, err)
	runCtx, stop := context.WithTimeout(ctx, 60*time.Second)
	defer stop()
	result, err := agent.Run(runCtx, &agents.AgentInput{
		Namespace: testutil.FixtureNamespace,
		ThreadID:  "thread-bg",
		StreamID:  agents.StreamIDForThread(testutil.FixtureNamespace, "thread-bg"),
		Message:   history.Message{Messages: []responses.InputMessageUnion{responses.UserMessage("index the docs")}},
	})
	require.NoError(t, err)
	require.Equal(t, agentstate.RunStatusCompleted, result.Status)

	eventually(t, 60*time.Second, func() error {
		if seen := f.Provider.Seen(); len(seen) < 3 {
			return fmt.Errorf("model called %d times, want the delivery run's call too", len(seen))
		}
		return nil
	})
	seen := f.Provider.Seen()
	assert.Contains(t, strings.Join(seen[2].Texts, "\n"), testutil.FixtureBackgroundResult, "%+v", seen)
	assert.Equal(t, 1, f.Calls.Of(testutil.FixtureBackgroundTool+":await"))
}

// TestRestateRunsAConfigBuiltSubAgent calls a sub-agent from a config-built
// agent on a real Restate server: the caller's step starts the sub-agent's own
// invocation and waits for it, and the answer comes back as the tool's result.
func TestRestateRunsAConfigBuiltSubAgent(t *testing.T) {
	f, _, ctx := startRestateBuilder(t,
		testutil.Step{Tool: testutil.FixtureAgent, Arguments: `{"message":"look it up"}`},
		testutil.Step{Text: "it is 42"},
		testutil.Step{Text: "the answer is 42"},
	)
	f.AddConfig(t)
	_, err := f.Store.Put(ctx, testutil.FixtureNamespace, agentconfig.Config{
		Name: "orchestrator", Model: testutil.FixtureModel, SubAgents: []agentconfig.SubAgent{{Name: testutil.FixtureAgent}},
	})
	require.NoError(t, err)
	agent, err := f.Agent(ctx, testutil.FixtureNamespace, "orchestrator")
	require.NoError(t, err)
	runCtx, stop := context.WithTimeout(ctx, 90*time.Second)
	defer stop()
	result, err := agent.Run(runCtx, &agents.AgentInput{
		Namespace: testutil.FixtureNamespace,
		ThreadID:  "thread-sub",
		Message:   history.Message{Messages: []responses.InputMessageUnion{responses.UserMessage("what is it?")}},
	})
	require.NoError(t, err)
	require.Equal(t, agentstate.RunStatusCompleted, result.Status)

	seen := f.Provider.Seen()
	require.Len(t, seen, 3, "%+v", seen)
	assert.Equal(t, []string{testutil.FixtureAgent}, seen[0].Tools)
	assert.Contains(t, strings.Join(seen[1].Texts, "\n"), "look it up")
	assert.Contains(t, strings.Join(seen[2].Texts, "\n"), "it is 42")
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func eventually(t *testing.T, timeout time.Duration, try func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		err := try()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("gave up after %s: %v", timeout, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
