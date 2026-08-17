package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/bytedance/sonic"
	"github.com/google/uuid"
	hastekit "github.com/hastekit/agent-sdk-go"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
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

	hist := hastekit.NewFileHistory("./conversations")
	agent := hastekit.NewAgent(&hastekit.AgentConfig{
		Name:        "Hello world agent",
		Instruction: hastekit.NewPrompt("You are helpful assistant."),
		LLM:         model,
		History:     hist,
	})

	threadID := uuid.New().String()

	handle, err := agent.Execute(context.Background(), &agents.AgentInput{
		Namespace: "default",
		ThreadID:  threadID,
		Message: history.Message{
			Messages: []responses.InputMessageUnion{
				responses.UserMessage("Hello! My name is Alice"),
			},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	out, err := handle.Result()
	if err != nil {
		log.Fatal(err)
	}

	b, _ := sonic.Marshal(out)
	fmt.Println(string(b))

	// Agent itself is stateless - you can either re-create another agent or reuse the same agent instance, but ensure to pass the correct `ThreadID`
	agent2 := hastekit.NewAgent(&hastekit.AgentConfig{
		Name:        "Hello world agent",
		Instruction: hastekit.NewPrompt("You are helpful assistant."),
		LLM:         model,
		History:     hist,
	})

	handle, err = agent2.Execute(context.Background(), &agents.AgentInput{
		Namespace: "default",
		ThreadID:  threadID,
		Message: history.Message{
			Messages: []responses.InputMessageUnion{
				responses.UserMessage("What's my name?"),
			},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	out, err = handle.Result()
	if err != nil {
		log.Fatal(err)
	}

	b, _ = sonic.Marshal(out)
	fmt.Println(string(b))
}
