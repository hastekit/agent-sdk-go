package main

import (
	"context"
	"log"
	"net/http"
	"os"

	hastekit "github.com/hastekit/agent-sdk-go"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
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
	fileHistory, err := hastekit.OpenFileHistory("./conversations")
	if err != nil {
		log.Fatal(err)
	}
	defer fileHistory.Close()

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

	history := fileHistory
	agentName := "SampleAgent"
	agent := hastekit.MustNewAgent(&hastekit.AgentConfig{
		Name:        agentName,
		Instruction: hastekit.NewPrompt("You are a helpful assistant. Use the get_user_name tool to get the user's name and greet them."),
		LLM:         model,
		History:     history,
		Tools: []agents.Tool{
			NewCustomTool(),
		},
	})

	registry := hastekit.NewRegistry()
	if err := registry.Register(agent); err != nil {
		log.Fatal(err)
	}
	if err := http.ListenAndServe(":8070", hastekit.NewHTTPHandler(registry)); err != nil {
		log.Fatal(err)
	}

	// You can then invoke by hitting POST http://localhost:8070/?agent=SampleAgent with `agents.AgentInput` as your payload
	/*
		  curl -X POST "http://localhost:8070/?agent=SampleAgent" \
		  -H "Content-Type: application/json" \
		  -d '{
			"messages": [
			  {
				"role": "user",
				"content": "Hello!"
			  }
			]
		  }'
	*/
}
