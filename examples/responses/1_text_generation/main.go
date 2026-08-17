package main

import (
	"context"
	"fmt"
	"log"
	"os"

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
			Instructions: utils.Ptr("You are helpful assistant. You greet user with a light-joke"),
			Input: responses.InputUnion{
				OfString: utils.Ptr("Hello!"),
			},
			Parameters: responses.Parameters{
				Temperature: utils.Ptr(0.2),
				ExtraFields: map[string]any{
					"additional_headers": map[string]string{
						"X-Custom-Header": "Custom Header Value",
					},
				},
			},
		},
	)
	if err != nil {
		log.Fatal(err)
	}

	acc := ""
	for chunk := range stream {
		if chunk.OfOutputTextDelta != nil {
			acc += chunk.OfOutputTextDelta.Delta
		}
	}
	fmt.Println(acc)
}
