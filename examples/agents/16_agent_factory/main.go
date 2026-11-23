// Agents built at runtime from stored, versioned configs instead of Go code.
//
// RUNTIME=local (default), temporal or restate picks where they run; the factory is the same.
// Temporal expects a server on localhost:7233. Restate expects ingress on localhost:8080 and
// this service registered with: restate deployments register http://localhost:9081
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

// storedAgent is one row of an agent_configs table. Rows are never edited: a change is a new version.
type storedAgent struct {
	Model       string
	Prompt      string
	Temperature float64
	Tools       []string
	Handoffs    []storedHandoff
}

type storedHandoff struct {
	Name, Version, Description string
}

// configStore stands in for the database: versioned configs plus a pointer to each agent's latest.
type configStore struct {
	mu      sync.RWMutex
	configs map[hastekit.AgentRef]storedAgent
	latest  map[string]string
}

func (s *configStore) Publish(name, version string, cfg storedAgent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.configs[hastekit.AgentRef{Name: name, Version: version}] = cfg
	s.latest[name] = version
}

func (s *configStore) Latest(name string) hastekit.AgentRef {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return hastekit.AgentRef{Name: name, Version: s.latest[name]}
}

func (s *configStore) Get(ref hastekit.AgentRef) (storedAgent, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cfg, ok := s.configs[ref]
	return cfg, ok
}

type CurrentTimeInput struct{}

func CurrentTime(context.Context, CurrentTimeInput) (string, error) {
	return time.Now().UTC().Format(time.RFC3339), nil
}

// factory turns a stored row into agent options. Secrets stay here: runs only carry the ref.
type factory struct {
	store   *configStore
	llm     *hastekit.LLMClient
	history *hastekit.History
	tools   map[string]hastekit.Tool
}

func (f *factory) Build(_ context.Context, ref hastekit.AgentRef) (*agents.AgentOptions, error) {
	cfg, ok := f.store.Get(ref)
	if !ok {
		return nil, fmt.Errorf("%w: %s", agents.ErrAgentNotFound, ref)
	}
	var tools []hastekit.Tool
	for _, name := range cfg.Tools {
		tool, ok := f.tools[name]
		if !ok {
			return nil, fmt.Errorf("agent %s uses unknown tool %q", ref, name)
		}
		tools = append(tools, tool)
	}
	var handoffs []*agents.Handoff
	for _, h := range cfg.Handoffs {
		// The target's version is part of this row, so this ref always builds the same graph.
		handoffs = append(handoffs, &agents.Handoff{Name: h.Name, Version: h.Version, Description: h.Description})
	}
	return (&hastekit.AgentConfig{
		Name:        ref.Name,
		LLM:         f.llm.Model(cfg.Model),
		Instruction: hastekit.NewPrompt(cfg.Prompt),
		Parameters:  hastekit.ModelParameters{Temperature: &cfg.Temperature},
		Tools:       tools,
		Handoffs:    handoffs,
		History:     f.history,
	}).Options(), nil
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	history, err := hastekit.OpenFileHistory("./conversations")
	if err != nil {
		log.Fatal(err)
	}
	defer history.Close()

	store := &configStore{configs: map[hastekit.AgentRef]storedAgent{}, latest: map[string]string{}}
	store.Publish("billing", "1", storedAgent{
		Model: "OpenAI/gpt-4.1-mini", Prompt: "You answer billing questions. Use current_time for dates.", Tools: []string{"current_time"},
	})
	store.Publish("support", "1", storedAgent{
		Model: "OpenAI/gpt-4.1-mini", Prompt: "You are support. Transfer billing questions to billing.",
		Handoffs: []storedHandoff{{Name: "billing", Version: "1", Description: "Billing and invoices"}},
	})

	llm := hastekit.NewLLMClient([]hastekit.ProviderConfig{{
		ProviderName: hastekit.ProviderOpenAI,
		ApiKeys:      []*hastekit.APIKeyConfig{{Name: "Key 1", APIKey: os.Getenv("OPENAI_API_KEY")}},
	}})
	agentFactory := &factory{store: store, llm: llm, history: history, tools: map[string]hastekit.Tool{
		"current_time": hastekit.NewTool(CurrentTime, hastekit.WithName("current_time"), hastekit.WithDescription("Returns the current UTC time")),
	}}

	// An in-memory broker is enough in one process; use NewRedisStreamBroker across processes.
	broker, _ := hastekit.NewStreamBroker()
	rt := newRuntime(ctx, broker, agentFactory)

	ask := func(question string) {
		ref := store.Latest("support")
		agent, err := rt.BuildAgent(ctx, ref)
		if err != nil {
			log.Fatal(err)
		}
		out, err := agent.Run(ctx, &hastekit.Input{Namespace: "default", Message: hastekit.UserTurn(question)})
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("[%s] %s\n", ref, out.Text())
	}

	ask("When is my next invoice due?")

	// Editing an agent publishes a new version. The next run uses it; no worker restart.
	store.Publish("support", "2", storedAgent{
		Model: "OpenAI/gpt-4.1-mini", Prompt: "You are support. Answer in one short sentence and transfer billing questions to billing.",
		Temperature: 0.2,
		Handoffs:    []storedHandoff{{Name: "billing", Version: "1", Description: "Billing and invoices"}},
	})
	ask("When is my next invoice due?")
}

// newRuntime registers the factory with the runtime RUNTIME names and starts serving it.
func newRuntime(ctx context.Context, broker agents.StreamBroker, f agents.AgentFactory) agents.FactoryRuntime {
	switch os.Getenv("RUNTIME") {
	case "temporal":
		rt, err := hastekit.NewTemporalRuntime("localhost:7233", broker)
		if err != nil {
			log.Fatal(err)
		}
		if err := rt.RegisterAgentFactory(f); err != nil {
			log.Fatal(err)
		}
		go func() {
			if err := rt.Serve(ctx); err != nil {
				log.Fatal(err)
			}
		}()
		return rt
	case "restate":
		rt, err := hastekit.NewRestateRuntime("http://localhost:8080", broker)
		if err != nil {
			log.Fatal(err)
		}
		if err := rt.RegisterAgentFactory(f); err != nil {
			log.Fatal(err)
		}
		go func() {
			if err := rt.Serve(ctx, "localhost:9081"); err != nil {
				log.Fatal(err)
			}
		}()
		return rt
	default:
		rt := local_runtime.NewLocalRuntime(broker)
		if err := rt.RegisterAgentFactory(f); err != nil {
			log.Fatal(err)
		}
		return rt
	}
}
