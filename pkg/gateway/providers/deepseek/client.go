// Package deepseek is the provider client for DeepSeek. The API is
// OpenAI-compatible on /chat/completions and has no /responses endpoint, so
// the native Responses calls run through the openaicompat bridge. Reasoning
// models return their chain of thought in `reasoning_content`, which the
// bridge maps onto native reasoning output items.
package deepseek

import (
	"net/http"

	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/providers/openaicompat"
)

const DefaultBaseURL = "https://api.deepseek.com"

type ClientOptions struct {
	// https://api.deepseek.com
	BaseURL string
	ApiKey  string
	Headers map[string]string

	Transport *http.Client
}

type Client = openaicompat.Client

func NewClient(opts *ClientOptions) *Client {
	baseURL := opts.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}

	return openaicompat.NewClient(&openaicompat.ClientOptions{
		BaseURL:   baseURL,
		ApiKey:    opts.ApiKey,
		Headers:   opts.Headers,
		Transport: opts.Transport,
	})
}
