package routines

import (
	"context"
	"errors"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/local_runtime"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
)

type runtimeFunc func(context.Context, *agents.Agent, *agents.AgentInput) (*agents.AgentOutput, error)

func (f runtimeFunc) Run(ctx context.Context, a *agents.Agent, in *agents.AgentInput) (*agents.AgentOutput, error) {
	return f(ctx, a, in)
}

type registry struct{ agent *agents.Agent }

func (r registry) Agent(name string) (*agents.Agent, bool) { return r.agent, name == r.agent.Name }
func (r registry) AgentNames() []string                    { return []string{r.agent.Name} }
func TestAgentExecutorDispatchesUserMessageAndTrustedContext(t *testing.T) {
	var input *agents.AgentInput
	agent := agents.NewAgent(&agents.AgentOptions{Name: "assistant", StreamBroker: streambroker.NewMemoryStreamBroker(), Runtime: runtimeFunc(func(_ context.Context, _ *agents.Agent, in *agents.AgentInput) (*agents.AgentOutput, error) {
		input = in
		return &agents.AgentOutput{Status: agentstate.RunStatusCompleted}, nil
	})})
	trusted := map[string]any{"credential": "trusted"}
	ex := &AgentExecutor{Registry: registry{agent}, ContextResolver: func(_ context.Context, r Routine) (map[string]any, error) {
		if r.Namespace != "tenant" {
			t.Fatal("wrong namespace")
		}
		return trusted, nil
	}}
	r := Routine{Definition: definition(), ID: "routine", Namespace: "tenant"}
	run := Run{ID: "occurrence", AgentRunID: "attempt"}
	if err := ex.Execute(context.Background(), r, run); err != nil {
		t.Fatal(err)
	}
	if input.GroupID != "routine" || input.Namespace != "tenant" || input.RunID != "attempt" || input.ThreadID != "occurrence" || input.SessionID != "occurrence" {
		t.Fatalf("%+v", input)
	}
	message := input.Message.Messages[0].OfInputMessage
	if message.Role != "user" || message.Content[0].OfInputText.Text != r.Instruction {
		t.Fatalf("%+v", message)
	}
	if input.RunContext["routine_id"] != "routine" || input.RunContext["routine_agent"] != "assistant" || input.RunContext["routine_occurrence_id"] != "occurrence" || input.RunContext["credential"] != "trusted" {
		t.Fatal(input.RunContext)
	}
	if len(trusted) != 1 {
		t.Fatal("mutated trusted context")
	}
	if !ex.HasAgent("assistant") || ex.HasAgent("missing") {
		t.Fatal("agent validation")
	}
	r.Agent = "missing"
	if err := ex.Execute(context.Background(), r, run); err == nil {
		t.Fatal("missing agent accepted")
	}
}
func TestAgentExecutorPropagatesPause(t *testing.T) {
	agent := agents.NewAgent(&agents.AgentOptions{Name: "assistant", StreamBroker: streambroker.NewMemoryStreamBroker(), Runtime: runtimeFunc(func(context.Context, *agents.Agent, *agents.AgentInput) (*agents.AgentOutput, error) {
		return &agents.AgentOutput{Status: agentstate.RunStatusPaused}, nil
	})})
	ex := &AgentExecutor{Registry: registry{agent}}
	if err := ex.Execute(context.Background(), Routine{Definition: definition()}, Run{}); !errors.Is(err, ErrPaused) {
		t.Fatal(err)
	}
}

func (runtimeFunc) RegisterAgent(*agents.AgentOptions) error { return nil }
func (runtimeFunc) StreamBroker() agents.StreamBroker        { return nil }

type groupTestLLM struct{}

func (groupTestLLM) NewStreamingResponses(context.Context, *agents.ModelCall, *responses.Request, func(*responses.ResponseChunk)) (*responses.Response, error) {
	return &responses.Response{}, nil
}

func TestRoutineExecutionPersistsGroup(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := history.NewFileConversationPersistence(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// Match stores containing legacy records with an empty run ID.
	if err := store.SaveMessages(ctx, "tenant", history.DefaultGroupID, "", "", "legacy-thread", "legacy-conversation", nil, nil); err != nil {
		t.Fatal(err)
	}
	broker := streambroker.NewMemoryStreamBroker()
	agent := agents.NewAgent(&agents.AgentOptions{Name: "assistant", History: history.NewConversationManager(store, history.WithMessageAttribution()), StreamBroker: broker, Runtime: local_runtime.NewLocalRuntime(broker), Middlewares: []agents.Middleware{agents.NoopMiddleware{}}}).WithLLM(groupTestLLM{})
	executor := &AgentExecutor{Registry: registry{agent}}
	_, cursor, err := broker.ReadRunEvents(ctx, []string{"tenant"}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	routine := Routine{Definition: definition(), ID: "routine-1", Namespace: "tenant"}
	if err := executor.Execute(ctx, routine, Run{ID: "occurrence-1", AgentRunID: "attempt-1"}); err != nil {
		t.Fatal(err)
	}
	normal, err := store.ListThreads(ctx, "tenant", "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(normal) != 1 || normal[0].ThreadID != "legacy-thread" {
		t.Fatalf("routine leaked into normal conversations: %+v", normal)
	}
	grouped, err := store.ListThreads(ctx, "tenant", routine.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(grouped) != 1 {
		t.Fatalf("missing routine conversation: %+v", grouped)
	}
	// A normal UI follow-up must retain the routine group.
	if _, err := agent.Run(ctx, &agents.AgentInput{Namespace: "tenant", GroupID: history.DefaultGroupID, ThreadID: "occurrence-1", Message: history.Message{Messages: []responses.InputMessageUnion{responses.UserMessage("Follow up")}}}); err != nil {
		t.Fatal(err)
	}
	// Both the initial routine run and its UI follow-up must notify the sidebar
	// using the persisted group, even when the follow-up input says default.
	events, _, err := broker.ReadRunEvents(ctx, []string{"tenant"}, cursor, 0)
	if err != nil || len(events) != 4 {
		t.Fatalf("expected start/finish events for both runs: %+v, %v", events, err)
	}
	for _, event := range events {
		if event.GroupID != routine.ID || event.ThreadID != "occurrence-1" {
			t.Fatalf("routine notification lost its group: %+v", event)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := history.NewFileConversationPersistence(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	normal, err = reopened.ListThreads(ctx, "tenant", history.DefaultGroupID)
	if err != nil || len(normal) != 1 || normal[0].ThreadID != "legacy-thread" {
		t.Fatalf("routine appeared in Recents after restart: %+v, %v", normal, err)
	}
	grouped, err = reopened.ListThreads(ctx, "tenant", routine.ID)
	if err != nil || len(grouped) != 1 {
		t.Fatalf("routine history missing after restart: %+v, %v", grouped, err)
	}
}
