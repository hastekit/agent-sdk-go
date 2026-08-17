package llm

import (
	"context"
	"slices"

	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/chat_completion"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/embeddings"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/image_edit"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/image_generation"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/speech"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/transcription"
)

type Provider interface {
	NewResponses(ctx context.Context, in *responses.Request) (*responses.Response, error)
	NewStreamingResponses(ctx context.Context, in *responses.Request) (chan *responses.ResponseChunk, error)
	NewEmbedding(ctx context.Context, in *embeddings.Request) (*embeddings.Response, error)
	NewChatCompletion(ctx context.Context, in *chat_completion.Request) (*chat_completion.Response, error)
	NewStreamingChatCompletion(ctx context.Context, in *chat_completion.Request) (chan *chat_completion.ResponseChunk, error)
	NewSpeech(ctx context.Context, in *speech.Request) (*speech.Response, error)
	NewStreamingSpeech(ctx context.Context, in *speech.Request) (chan *speech.ResponseChunk, error)
	NewTranscription(ctx context.Context, in *transcription.Request) (*transcription.Response, error)
	NewImageGeneration(ctx context.Context, in *image_generation.Request) (*image_generation.Response, error)
	NewImageEdit(ctx context.Context, in *image_edit.Request) (*image_edit.Response, error)
}

type ProviderName string

var (
	ProviderNameOpenAI     ProviderName = "OpenAI"
	ProviderNameAnthropic  ProviderName = "Anthropic"
	ProviderNameGemini     ProviderName = "Gemini"
	ProviderNameXAI        ProviderName = "xAI"
	ProviderNameOllama     ProviderName = "Ollama"
	ProviderNameOpenRouter ProviderName = "OpenRouter"
	ProviderNameElevenLabs ProviderName = "ElevenLabs"
	ProviderNameBedrock    ProviderName = "Bedrock"
	ProviderNameSarvam     ProviderName = "Sarvam"
	ProviderNameDeepSeek   ProviderName = "DeepSeek"
	ProviderNameMoonshot   ProviderName = "Moonshot"
	ProviderNameZAI        ProviderName = "Z.ai"
)

func GetAllProviderNames() []ProviderName {
	return []ProviderName{
		ProviderNameOpenAI,
		ProviderNameAnthropic,
		ProviderNameGemini,
		ProviderNameXAI,
		ProviderNameOllama,
		ProviderNameOpenRouter,
		ProviderNameElevenLabs,
		ProviderNameBedrock,
		ProviderNameSarvam,
		ProviderNameDeepSeek,
		ProviderNameMoonshot,
		ProviderNameZAI,
	}
}

func (p *ProviderName) IsValid() bool {
	return slices.Contains(GetAllProviderNames(), *p)
}
