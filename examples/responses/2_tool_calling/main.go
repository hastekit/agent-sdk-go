package main

import (
	"context"
	"fmt"
	"log"
	"os"

	json "github.com/bytedance/sonic"
	hastekit "github.com/hastekit/agent-sdk-go"
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
	stream, err := model.NewStreamingResponses(
		context.Background(),
		&responses.Request{
			Instructions: utils.Ptr("You are helpful assistant. You will greet the user by their name."),
			Input: responses.InputUnion{
				OfString: utils.Ptr("Hello!"),
			},
			Tools: []responses.ToolUnion{
				{
					OfFunction: &responses.FunctionTool{
						Name:        "get_user_name",
						Description: utils.Ptr("This tool returns the user's name"),
					},
				},
			},
		},
	)
	if err != nil {
		log.Fatal(err)
	}

	var fnCalls []*responses.FunctionCallMessage
	for chunk := range stream {
		switch chunk.ChunkType() {
		case "response.output_item.done":
			if chunk.OfOutputItemDone.Item.Type == "function_call" {
				fnCall := &responses.FunctionCallMessage{
					ID:        chunk.OfOutputItemDone.Item.Id,
					CallID:    *chunk.OfOutputItemDone.Item.CallID,
					Name:      *chunk.OfOutputItemDone.Item.Name,
					Arguments: *chunk.OfOutputItemDone.Item.Arguments,
				}
				fnCalls = append(fnCalls, fnCall)
			}
		}
	}

	// Handle function calls
	t, _ := json.Marshal(fnCalls)
	fmt.Println(string(t))
}
