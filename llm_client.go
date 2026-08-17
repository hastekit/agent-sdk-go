package sdk

import (
	"strings"

	"github.com/hastekit/hastekit-sdk-go/pkg/gateway"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm"
)

type ProviderConfig = gateway.ProviderConfig
type APIKeyConfig = gateway.APIKeyConfig

type ProviderName = llm.ProviderName

var (
	ProviderOpenAI     = llm.ProviderNameOpenAI
	ProviderAnthropic  = llm.ProviderNameAnthropic
	ProviderBedrock    = llm.ProviderNameBedrock
	ProviderElevenLabs = llm.ProviderNameElevenLabs
	ProviderGemini     = llm.ProviderNameGemini
	ProviderXAI        = llm.ProviderNameXAI
	ProviderOllama     = llm.ProviderNameOllama
	ProviderOpenRouter = llm.ProviderNameOpenRouter
	ProviderSarvam     = llm.ProviderNameSarvam
	ProviderDeepSeek   = llm.ProviderNameDeepSeek
	ProviderMoonshot   = llm.ProviderNameMoonshot // Kimi models
	ProviderZAI        = llm.ProviderNameZAI      // GLM models
)

type LLMClient struct {
	providerConfigs []ProviderConfig
	llmGateway      *gateway.InternalLLMGateway
}

func NewLLMClient(configs []ProviderConfig) *LLMClient {
	gw := gateway.NewLLMGateway(gateway.NewInMemoryConfigStore(configs))
	gw.UseMiddleware(gateway.NewTracingMiddleware())

	return &LLMClient{
		providerConfigs: configs,
		llmGateway:      gateway.NewInternalLLMGateway(gw),
	}
}

type Model struct {
	modelId string
	client  llm.Provider
}

func (c *LLMClient) Model(id string) llm.Provider {
	i := strings.SplitN(id, "/", 2)

	return gateway.NewLLMClient(
		c.llmGateway,
		gateway.NewInMemoryConfigStore(c.providerConfigs), gateway.WithModel(llm.ProviderName(i[0]), i[1]),
	)
}
