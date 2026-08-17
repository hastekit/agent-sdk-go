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

	agent1 := hastekit.NewAgent(&hastekit.AgentConfig{
		Name:        "JokeAgent",
		Instruction: hastekit.NewPrompt("You are joke teller"),
		LLM:         model,
		History:     hastekit.NewFileHistory("./conversations"),
	})

	agent2 := hastekit.NewAgent(&hastekit.AgentConfig{
		Name:        "FactAgent",
		Instruction: hastekit.NewPrompt("You are a fact teller"),
		LLM:         model,
		History:     hastekit.NewFileHistory("./conversations"),
	})

	routerAgent := hastekit.NewAgent(&hastekit.AgentConfig{
		Name:        "RouterAgent",
		Instruction: hastekit.NewPrompt("You are router agent. You must not respond directly. Your role is only to delegate to other agents"),
		LLM:         model,
		Handoffs: []*agents.Handoff{
			agents.NewHandoff(agent1.Name, "Use this agent to generate jokes", agent1),
			agents.NewHandoff(agent2.Name, "Use this agent to generate facts", agent2),
		},
		History: hastekit.NewFileHistory("./conversations"),
	})

	handle, err := routerAgent.Execute(context.Background(), &agents.AgentInput{
		Message: history.Message{
			Messages: []responses.InputMessageUnion{
				responses.UserMessage("Hello! Tell me a joke about universe"),
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
}
