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

type CustomTool struct {
	*agents.BaseTool
}

func NewCustomTool() *CustomTool {
	return &CustomTool{
		BaseTool: &agents.BaseTool{
			ToolUnion: responses.ToolUnion{
				OfFunction: &responses.FunctionTool{
					Name:        "get_user_name",
					Description: utils.Ptr("Returns the user's name"),
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"user_id": map[string]any{
								"type":        "string",
								"description": "The user ID to look up",
							},
						},
						"required": []string{"user_id"},
					},
				},
			},
		},
	}
}

func (t *CustomTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	return &agents.ToolCallResponse{
		FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			ID:     params.ID,
			CallID: params.CallID,
			Output: responses.FunctionCallOutputContentUnion{
				OfString: utils.Ptr("Bob"),
			},
		},
		StateUpdates: map[string]string{},
	}, nil
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
			NewCustomTool(),
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
