// Package zai is the provider client for Z.ai (Zhipu AI), which serves the
// GLM model family. The API is OpenAI-compatible on /chat/completions and has
// no /responses endpoint, so the native Responses calls run through the
// openaicompat bridge.
//
// The default base URL is Z.ai's general API endpoint. Coding Plan keys are
// served from https://api.z.ai/api/coding/paas/v4 and Zhipu (mainland China)
// accounts from https://open.bigmodel.cn/api/paas/v4 - both are set through
// BaseURL.
package zai

import (
	"net/http"

	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/providers/openaicompat"
)

const DefaultBaseURL = "https://api.z.ai/api/paas/v4"

type ClientOptions struct {
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
