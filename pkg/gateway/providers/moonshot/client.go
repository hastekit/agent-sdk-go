// Package moonshot is the provider client for Moonshot AI, which serves the
// Kimi model family. The API is OpenAI-compatible on /chat/completions and has
// no /responses endpoint, so the native Responses calls run through the
// openaicompat bridge.
//
// The default base URL is the global endpoint; mainland China accounts point
// BaseURL at https://api.moonshot.cn/v1 instead.
package moonshot

import (
	"net/http"

	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/providers/openaicompat"
)

const DefaultBaseURL = "https://api.moonshot.ai/v1"

type ClientOptions struct {
	// https://api.moonshot.ai/v1
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
