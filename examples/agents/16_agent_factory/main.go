// Agents defined by stored, versioned specs instead of Go code.
//
// RUNTIME=local (default) or temporal picks where they run; the factory is the same.
// Temporal expects a server on localhost:7233.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	hastekit "github.com/hastekit/agent-sdk-go"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/local_runtime"
)

// specStore stands in for an agent_configs table. Rows are never edited: a change is a new version.
type specStore struct {
	mu     sync.RWMutex
	specs  map[hastekit.AgentRef]hastekit.AgentSpec
	latest map[string]string
}

func (s *specStore) Publish(spec hastekit.AgentSpec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.specs[spec.Ref()] = spec
	s.latest[spec.Name] = spec.Version
}

// Load is the factory's loader. An empty version means the latest.
func (s *specStore) Load(_ context.Context, ref hastekit.AgentRef) (*hastekit.AgentSpec, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if ref.Version == "" {
		ref.Version = s.latest[ref.Name]
	}
	spec, ok := s.specs[ref]
	if !ok {
		return nil, fmt.Errorf("%w: %s", hastekit.ErrAgentNotFound, ref)
	}
	return &spec, nil
}

type CurrentTimeInput struct{}

func CurrentTime(context.Context, CurrentTimeInput) (string, error) {
	return time.Now().UTC().Format(time.RFC3339), nil
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	history, err := hastekit.OpenFileHistory("./conversations")
	if err != nil {
		log.Fatal(err)
	}
	defer history.Close()

	store := &specStore{specs: map[hastekit.AgentRef]hastekit.AgentSpec{}, latest: map[string]string{}}
	store.Publish(hastekit.AgentSpec{
		Name: "billing", Version: "1", Model: "OpenAI/gpt-4.1-mini",
		Instruction: "You answer billing questions. Use current_time for dates.",
		Tools:       []string{"current_time"},
	})
	store.Publish(hastekit.AgentSpec{
		Name: "support", Version: "1", Model: "OpenAI/gpt-4.1-mini",
		Instruction: "You are support. Transfer billing questions to billing.",
		Handoffs:    []hastekit.HandoffSpec{{Name: "billing", Version: "1", Description: "Billing and invoices"}},
	})

	// The factory pairs the store with what specs name. Keys and code stay here; runs carry only the spec.
	factory := &hastekit.AgentFactory{
		Load: store.Load,
		Models: hastekit.NewLLMClient([]hastekit.ProviderConfig{{
			ProviderName: hastekit.ProviderOpenAI,
			ApiKeys:      []*hastekit.APIKeyConfig{{Name: "Key 1", APIKey: os.Getenv("OPENAI_API_KEY")}},
		}}),
		Tools: []hastekit.Tool{
			hastekit.NewTool(CurrentTime, hastekit.WithName("current_time"), hastekit.WithDescription("Returns the current UTC time")),
		},
		History: history,
	}

	// An in-memory broker is enough in one process; use NewRedisStreamBroker across processes.
	broker, _ := hastekit.NewStreamBroker()
	rt := newRuntime(ctx, broker, factory)

	ask := func(question string) {
		agent, err := rt.BuildAgent(ctx, hastekit.AgentRef{Name: "support"})
		if err != nil {
			log.Fatal(err)
		}
		out, err := agent.Run(ctx, &hastekit.Input{Namespace: "default", Message: hastekit.UserTurn(question)})
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("[support@%s] %s\n", agent.Spec().Version, out.Text())
	}

	ask("When is my next invoice due?")

	// Editing an agent publishes a new version. The next run uses it; no worker restart.
	store.Publish(hastekit.AgentSpec{
		Name: "support", Version: "2", Model: "OpenAI/gpt-4.1-mini",
		Instruction: "You are support. Answer in one short sentence and transfer billing questions to billing.",
		Handoffs:    []hastekit.HandoffSpec{{Name: "billing", Version: "1", Description: "Billing and invoices"}},
	})
	ask("When is my next invoice due?")
}

// newRuntime registers the factory with the runtime RUNTIME names and starts serving it.
func newRuntime(ctx context.Context, broker agents.StreamBroker, factory *hastekit.AgentFactory) agents.FactoryRuntime {
	switch os.Getenv("RUNTIME") {
	case "temporal":
		rt, err := hastekit.NewTemporalRuntime("localhost:7233", broker)
		if err != nil {
			log.Fatal(err)
		}
		if err := rt.RegisterAgentFactory(factory); err != nil {
			log.Fatal(err)
		}
		go func() {
			if err := rt.Serve(ctx); err != nil {
				log.Fatal(err)
			}
		}()
		return rt
	default:
		rt := local_runtime.NewLocalRuntime(broker)
		if err := rt.RegisterAgentFactory(factory); err != nil {
			log.Fatal(err)
		}
		return rt
	}
}
