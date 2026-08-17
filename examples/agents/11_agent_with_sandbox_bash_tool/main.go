package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/bytedance/sonic"
	hastekit "github.com/hastekit/agent-sdk-go"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/tools"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/hastekitgateway"
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

	model := client.Model("OpenAI/gpt-5-mini")

	sandboxCfg := &hastekitgateway.Config{
		Endpoint:   "http://localhost:6060",
		HttpClient: http.DefaultClient,
	}

	agent := hastekit.NewAgent(&hastekit.AgentConfig{
		Name:        "Hello world agent",
		Instruction: hastekit.NewPrompt("You are helpful assistant."),
		LLM:         model,
		Tools:       []agents.Tool{tools.NewBashTool(sandboxCfg.NewSandboxClient(), "hastekit-ai-sandbox:v1", nil)},
	})

	handle, err := agent.Execute(context.Background(), &agents.AgentInput{
		Message: history.Message{
			Messages: []responses.InputMessageUnion{
				responses.UserMessage("what is the current time? use bash tool"),
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
