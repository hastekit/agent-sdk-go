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
	"github.com/hastekit/agent-sdk-go/pkg/agents/tools"
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

	agentTool := tools.NewAgentTool(
		"get_user_name",
		"Returns the user's name",
		hastekit.NewAgent(&hastekit.AgentConfig{
			Name:        "Hello world agent",
			Instruction: hastekit.NewPrompt("You are helpful assistant."),
			LLM:         model,
		}),
		tools.SubAgentContextModeNone,
	)

	agent := hastekit.NewAgent(&hastekit.AgentConfig{
		Name:        "Hello world agent",
		Instruction: hastekit.NewPrompt("You are helpful assistant."),
		LLM:         model,
		Tools:       []agents.Tool{agentTool},
	})

	handle, err := agent.Execute(context.Background(), &agents.AgentInput{
		Message: history.Message{
			Messages: []responses.InputMessageUnion{
				responses.UserMessage("what is the username of user_id '123'?"),
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
