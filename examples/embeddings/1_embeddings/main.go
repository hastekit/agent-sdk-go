package main

import (
	"context"
	"fmt"
	"log"
	"os"

	json "github.com/bytedance/sonic"
	hastekit "github.com/hastekit/agent-sdk-go"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/embeddings"
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
		{
			ProviderName: hastekit.ProviderGemini,
			ApiKeys: []*hastekit.APIKeyConfig{
				{
					Name:   "Key 1",
					APIKey: os.Getenv("GEMINI_API_KEY"),
				},
			},
		},
	})

	model := client.Model("OpenAI/text-embedding-ada-002")
	resp, err := model.NewEmbedding(context.Background(), &embeddings.Request{
		Input: embeddings.InputUnion{
			OfString: utils.Ptr("The food was delicious and the waiter..."),
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	b, _ := json.Marshal(resp)
	fmt.Println(string(b))
}
