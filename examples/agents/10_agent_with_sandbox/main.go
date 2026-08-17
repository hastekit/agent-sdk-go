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

	model := client.Model("OpenAI/gpt-4.1-mini")

	// The sandbox manager talks to the HasteKit sandbox service.
	sandboxCfg := &hastekitgateway.Config{
		Endpoint:   "https://app.hastekit.ai",
		HttpClient: http.DefaultClient,
	}

	hist := hastekit.NewFileHistory("./conversations")
	agent := hastekit.NewAgent(&hastekit.AgentConfig{
		Name:        "hello-world-agent",
		Instruction: hastekit.NewPrompt("You are a helpful assistant with access to terminal (bash)"),
		LLM:         model,
		History:     hist,
		Tools: []agents.Tool{
			tools.NewBashTool(sandboxCfg.NewSandboxClient(), "praveenraj9495/hastekit-ai-sandbox:latest", map[string]string{}),
		},
	})

	handle, err := agent.Execute(context.Background(), &agents.AgentInput{
		Message: history.Message{
			Messages: []responses.InputMessageUnion{
				responses.UserMessage("What is the current time?"),
			},
		},
		Namespace:         "default",
		ThreadID:          "",
		PreviousMessageID: "",
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
