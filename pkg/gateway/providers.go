package gateway

import (
	"context"
	"fmt"

	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/providers/anthropic"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/providers/bedrock"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/providers/deepseek"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/providers/elevenlabs"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/providers/gemini"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/providers/moonshot"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/providers/openai"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/providers/sarvam"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/providers/xai"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/providers/zai"
)

func (g *LLMGateway) getProvider(ctx context.Context, providerName llm.ProviderName, req *llm.Request, key string) (llm.Provider, error) {
	var baseUrl string
	var customHeaders map[string]string

	providerConfig, err := g.ConfigStore.GetProviderConfig(ctx, providerName, key)
	if err != nil {
		return nil, err
	}

	if providerConfig != nil {
		baseUrl = providerConfig.BaseURL
		customHeaders = providerConfig.CustomHeaders
	}

	switch providerName {
	case llm.ProviderNameOpenAI:
		return openai.NewClient(&openai.ClientOptions{
			BaseURL: baseUrl,
			ApiKey:  key,
			Headers: customHeaders,
		}), nil

	case llm.ProviderNameAnthropic:
		return anthropic.NewClient(&anthropic.ClientOptions{
			BaseURL: baseUrl,
			ApiKey:  key,
			Headers: customHeaders,
		}), nil

	case llm.ProviderNameGemini:
		return gemini.NewClient(&gemini.ClientOptions{
			BaseURL: baseUrl,
			ApiKey:  key,
			Headers: customHeaders,
		}), nil

	case llm.ProviderNameXAI:
		return xai.NewClient(&xai.ClientOptions{
			BaseURL: baseUrl,
			ApiKey:  key,
			Headers: customHeaders,
		}), nil
	case llm.ProviderNameOllama:
		return openai.NewClient(&openai.ClientOptions{
			BaseURL: baseUrl,
			ApiKey:  key,
			Headers: customHeaders,
		}), nil

	case llm.ProviderNameOpenRouter:
		if baseUrl == "" {
			baseUrl = "https://openrouter.ai/api/v1"
		}
		return openai.NewClient(&openai.ClientOptions{
			BaseURL: baseUrl,
			ApiKey:  key,
			Headers: customHeaders,
		}), nil

	case llm.ProviderNameElevenLabs:
		return elevenlabs.NewClient(&elevenlabs.ClientOptions{
			BaseURL: baseUrl,
			ApiKey:  key,
			Headers: customHeaders,
		}), nil

	case llm.ProviderNameBedrock:
		return bedrock.NewClient(&bedrock.ClientOptions{
			BaseURL: baseUrl,
			ApiKey:  key,
			Headers: customHeaders,
		}), nil

	case llm.ProviderNameSarvam:
		return sarvam.NewClient(&sarvam.ClientOptions{
			BaseURL: baseUrl,
			ApiKey:  key,
			Headers: customHeaders,
		}), nil

	case llm.ProviderNameDeepSeek:
		return deepseek.NewClient(&deepseek.ClientOptions{
			BaseURL: baseUrl,
			ApiKey:  key,
			Headers: customHeaders,
		}), nil

	case llm.ProviderNameMoonshot:
		return moonshot.NewClient(&moonshot.ClientOptions{
			BaseURL: baseUrl,
			ApiKey:  key,
			Headers: customHeaders,
		}), nil

	case llm.ProviderNameZAI:
		return zai.NewClient(&zai.ClientOptions{
			BaseURL: baseUrl,
			ApiKey:  key,
			Headers: customHeaders,
		}), nil
	}

	return nil, fmt.Errorf("unknown provider: %s", providerName)
}
