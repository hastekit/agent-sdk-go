package main

import (
	"log"
	"net/http"
	"os"

	hastekit "github.com/hastekit/agent-sdk-go"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/tools"
	"github.com/joho/godotenv"
)

func main() {
	if err := godotenv.Load(".env"); err != nil {
		log.Fatal("Error loading .env file")
	}

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
	rt, err := hastekit.NewTemporalRuntime("0.0.0.0:7233", "localhost:6379")
	if err != nil {
		log.Fatal(err)
	}
	broker, err := hastekit.NewRedisStreamBroker("localhost:6379")
	if err != nil {
		log.Fatal(err)
	}

	agentName := "SampleAgent"
	_ = hastekit.NewAgent(&hastekit.AgentConfig{
		Name:        agentName,
		Instruction: hastekit.NewPrompt("You are helpful assistant. You are interacting with the user named {{name}}"),
		LLM:         model,
		History:     hastekit.NewFileHistory("./conversations"),
		Tools: []agents.Tool{
			tools.NewAgentTool(
				"joke-generator-agent",
				"Use to generate jokes",
				hastekit.NewAgent(&hastekit.AgentConfig{
					Name:        "joke-generator",
					Instruction: hastekit.NewPrompt("You are helpful assistant."),
					LLM:         model,
					History:     hastekit.NewFileHistory("./conversations"),
				}, hastekit.WithRuntime(rt, broker)),
				tools.SubAgentContextModeNone,
			),
		},
	}, hastekit.WithRuntime(rt, broker))

	go rt.Start()                                                 // Do this on the temporal service
	err = http.ListenAndServe(":8070", hastekit.NewHTTPHandler()) // Do this on the application that invokes the temporal workflow
	if err != nil {
		log.Fatal(err)
	}
}
