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
			Instructions: utils.Ptr("You are a personal math tutor. When asked a math question, write and run code using the python tool to answer the question."),
			Input: responses.InputUnion{
				OfInputMessageList: responses.InputMessageList{
					{
						OfEasyInput: &responses.EasyMessage{
							Role: constants.RoleUser,
							Content: responses.EasyInputContentUnion{
								OfString: utils.Ptr("I need to solve the equation 3x + 11 = 14. Can you help me?"),
							},
						},
					},
				},
			},
			Tools: []responses.ToolUnion{
				{
					OfCodeExecution: &responses.CodeExecutionTool{
						Container: &responses.CodeExecutionToolContainerUnion{
							ContainerConfig: &responses.CodeExecutionToolContainerConfig{
								Type:        "auto",
								MemoryLimit: "4g",
							},
						},
					},
				},
			},
			Parameters: responses.Parameters{
				Include: []responses.Includable{
					responses.IncludableCodeInterpreterCallOutputs,
				},
			},
		},
	)
	if err != nil {
		log.Fatal(err)
	}

	for chunk := range stream {
		if chunk.OfOutputItemDone != nil {
			if chunk.OfOutputItemDone.Item.Type == "code_interpreter_call" {
				fmt.Println(chunk.OfOutputItemDone.Item.Code, chunk.OfOutputItemDone.Item.Outputs)
			}
		}
	}
}
