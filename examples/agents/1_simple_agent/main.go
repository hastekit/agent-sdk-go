package main

import (
	"context"
	"fmt"
	hastekit "github.com/hastekit/agent-sdk-go"
	"log"
	"os"
)

func main() {
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

	agent, err := hastekit.NewAgent(&hastekit.AgentConfig{
		Name: "Assistant", LLM: model, Instruction: hastekit.NewPrompt("You are a helpful assistant."),
	})
	if err != nil {
		log.Fatal(err)
	}
	result, err := agent.Run(context.Background(), &hastekit.Input{
		Message: hastekit.UserTurn("Hello!"),
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(result.Text())
}
