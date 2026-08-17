package gateway

import (
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
)

// ProviderConfig contains provider-level configuration.
// This is the gateway's own type, independent of services layer.
type ProviderConfig struct {
	ProviderName  llm.ProviderName
	BaseURL       string
	CustomHeaders map[string]string
	ApiKeys       []*APIKeyConfig
}

// APIKeyConfig contains API key information for a provider.
// This is the gateway's own type, independent of services layer.
type APIKeyConfig struct {
	ProviderName llm.ProviderName
	APIKey       string
	Name         string
	RateLimits   []RateLimit
	Weight       int
	Enabled      bool
	IsDefault    bool
}

// VirtualKeyConfig contains virtual key access configuration.
// This is the gateway's own type, independent of services layer.
type VirtualKeyConfig struct {
	SecretKey        string
	AllowedProviders []llm.ProviderName
	AllowedModels    []string
	RateLimits       []RateLimit
}

type RateLimit struct {
	Unit  string `json:"unit"`
	Limit int64  `json:"limit"`
}
