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

// GetUserNameInput is the tool's argument. The field tags are what the model
// is shown: NewTool derives the tool's JSON schema from this struct, so there
// is no hand-written parameter schema to keep in step with the function.
type GetUserNameInput struct {
	UserID string `json:"user_id" jsonschema_description:"The user ID to look up"`
}

type GetUserNameOutput struct {
	Name string `json:"name"`
}

// GetUserName is an ordinary Go function. NewTool turns it into a tool: it
// unmarshals the model's arguments into the input struct, calls this, and
// marshals what comes back as the tool's result.
func GetUserName(ctx context.Context, in GetUserNameInput) (GetUserNameOutput, error) {
	return GetUserNameOutput{Name: "Bob"}, nil
}

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
		Instruction: hastekit.NewPrompt("You are a helpful assistant. Use the get_user_name tool to get the user's name and greet them."),
		LLM:         model,
		History:     hist,
		Tools: []agents.Tool{
			hastekit.NewTool(GetUserName,
				hastekit.WithName("get_user_name"),
				hastekit.WithDescription("Returns the user's name"),
				hastekit.WithReadOnly(true),
			),
		},
	})

	handle, err := agent.Execute(context.Background(), &agents.AgentInput{
		Message: history.Message{
			Messages: []responses.InputMessageUnion{
				responses.UserMessage("Hello! Can you get my name for user_id '123'?"),
			},
		},
		Namespace:     "default",
		ThreadID:      "",
		PreviousRunID: "",
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
