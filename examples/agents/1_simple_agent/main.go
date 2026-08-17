package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/bytedance/sonic"
	hastekit "github.com/hastekit/agent-sdk-go"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
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

	agent := hastekit.NewAgent(&hastekit.AgentConfig{
		Name:        "Hello world agent",
		Instruction: hastekit.NewPrompt("You are helpful assistant. You are interacting with the user named {{name}}"),
		LLM:         model,
		Parameters: responses.Parameters{
			Temperature: utils.Ptr(0.2),
		},
	})

	handle, err := agent.Execute(context.Background(), &agents.AgentInput{
		Message: history.Message{
			Messages: []responses.InputMessageUnion{
				responses.UserMessage("Hello!"),
			},
		},
		RunContext: map[string]any{
			"name": "Bob",
		},
		Namespace: "default",
		ThreadID:  "test123",
	})
	if err != nil {
		log.Fatal(err)
	}

	for chunk := range handle.Chunks {
		b, _ := sonic.Marshal(chunk)
		fmt.Println(string(b))
	}
}
