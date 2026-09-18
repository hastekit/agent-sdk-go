package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"

	hastekit "github.com/hastekit/agent-sdk-go"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/tools"
)

func main() {
	fileHistory, err := hastekit.OpenFileHistory("./conversations")
	if err != nil {
		log.Fatal(err)
	}
	defer fileHistory.Close()

	shutdownTelemetry := NewProvider(os.Getenv("LANGFUSE_BASE_URL"))
	defer shutdownTelemetry()

	client := hastekit.NewLLMClient([]hastekit.ProviderConfig{
		{
			ProviderName: hastekit.ProviderOpenAI,
			ApiKeys: []*hastekit.APIKeyConfig{
				{
					Name:   "Key 1",
					APIKey: os.Getenv("OPENAI_API_KEY"),
				},
			},
		},
	})

	model := client.Model("OpenAI/gpt-4.1-mini")

	// Temporal server endpoint + Redis for streaming
	// The broker carries the run's stream; the runtime is built around it.
	broker, err := hastekit.NewRedisStreamBroker("localhost:6379", "", 0)
	if err != nil {
		log.Fatal(err)
	}
	rt, err := hastekit.NewTemporalRuntime("0.0.0.0:7233", broker)
	if err != nil {
		log.Fatal(err)
	}

	defer rt.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	specialist, err := hastekit.NewAgent(&hastekit.AgentConfig{
		Name: "joke-generator", LLM: model, Instruction: hastekit.NewPrompt("You are a helpful assistant."), History: fileHistory,
	}, hastekit.WithRuntime(rt))
	if err != nil {
		log.Fatal(err)
	}
	agent, err := hastekit.NewAgent(&hastekit.AgentConfig{
		Name: "SampleAgent", LLM: model, Instruction: hastekit.NewPrompt("You are a helpful assistant."), History: fileHistory,
		Tools: []agents.Tool{tools.NewAgentTool("joke-generator-agent", "Use to generate jokes", specialist, tools.SubAgentContextModeNone)},
	}, hastekit.WithRuntime(rt))
	if err != nil {
		log.Fatal(err)
	}
	registry := hastekit.NewRegistry()
	if err := registry.Register(agent); err != nil {
		log.Fatal(err)
	}
	// Worker and HTTP serving can also run in separate processes.
	go func() {
		if err := rt.Serve(ctx); err != nil {
			log.Print(err)
			stop()
		}
	}()
	httpServer := &http.Server{Addr: ":8070", Handler: hastekit.NewHTTPHandler(registry)}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	err = httpServer.ListenAndServe()
	if err != nil && err != http.ErrServerClosed {
		log.Print(err)
	}
}
