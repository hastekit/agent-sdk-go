package main

import (
	"context"
	"fmt"
	"log"
	"os"

	hastekit "github.com/hastekit/agent-sdk-go"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
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
			Instructions: utils.Ptr("Describe this image"),
			Input: responses.InputUnion{
				OfInputMessageList: responses.InputMessageList{
					{
						OfEasyInput: &responses.EasyMessage{
							Role: constants.RoleUser,
							Content: responses.EasyInputContentUnion{
								OfString: utils.Ptr("Describe this image"),
							},
						},
					},
					{
						OfInputMessage: &responses.InputMessage{
							Role: constants.RoleUser,
							Content: responses.InputContent{
								{
									OfInputImage: &responses.InputImageContent{
										ImageURL: utils.Ptr("https://picsum.photos/200/300"),
										Detail:   "auto",
									},
								},
							},
						},
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
