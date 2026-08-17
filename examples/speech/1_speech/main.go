package main

import (
	"context"
	"log"
	"os"

	hastekit "github.com/hastekit/agent-sdk-go"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/speech"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

func main() {
	// Initialize SDK client
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

	// Generate speech
	model := client.Model("OpenAI/tts-1")
	resp, err := model.NewSpeech(context.Background(), &speech.Request{
		Input:          "Hello! This is a text-to-speech example using HasteKit SDK.",
		Voice:          "alloy",
		ResponseFormat: utils.Ptr("mp3"),
	})
	if err != nil {
		log.Fatal(err)
	}

	// Save audio file
	err = os.WriteFile("output.mp3", resp.Audio, 0644)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("Audio generated successfully! Size: %d bytes, Type: %s\n",
		len(resp.Audio), resp.ContentType)
}
