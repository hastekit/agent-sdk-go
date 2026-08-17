package gateway

import (
	"context"
	"fmt"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
)

// InMemoryConfigStore implements gateway.ConfigStore for SDK use.
// It holds API keys and provider configs in memory.
type InMemoryConfigStore struct {
	providerConfigs map[llm.ProviderName]*ProviderConfig
}

// NewInMemoryConfigStore creates a config store with full provider options.
func NewInMemoryConfigStore(configs []ProviderConfig) *InMemoryConfigStore {
	store := &InMemoryConfigStore{
		providerConfigs: make(map[llm.ProviderName]*ProviderConfig),
	}

	for _, config := range configs {
		// Set provider config
		store.providerConfigs[config.ProviderName] = &ProviderConfig{
			ProviderName:  config.ProviderName,
			BaseURL:       config.BaseURL,
			CustomHeaders: config.CustomHeaders,
			ApiKeys:       config.ApiKeys,
		}
	}

	return store
}

func (s *InMemoryConfigStore) GetProviderConfig(_ context.Context, providerName llm.ProviderName, key string) (*ProviderConfig, error) {
	config := s.providerConfigs[providerName]

	if config == nil || len(config.ApiKeys) == 0 {
		return nil, fmt.Errorf("no API key configured for provider %s", providerName)
	}

	return config, nil
}

func (s *InMemoryConfigStore) GetVirtualKey(_ context.Context, secretKey string) (*VirtualKeyConfig, error) {
	// In-memory store doesn't support virtual keys - they're managed by agent-server
	return nil, fmt.Errorf("virtual keys are not supported in direct mode")
}
