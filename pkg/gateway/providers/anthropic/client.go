package anthropic

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/bytedance/sonic"
	responses2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	anthropic_responses2 "github.com/hastekit/agent-sdk-go/pkg/gateway/providers/anthropic/anthropic_responses"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/base"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

type ClientOptions struct {
	// https://api.openai.com/v1
	BaseURL string
	ApiKey  string
	Headers map[string]string

	// HTTPClient allows callers to configure timeouts and transports.
	HTTPClient *http.Client

	transport *http.Client
}

type Client struct {
	*base.BaseProvider
	opts *ClientOptions
}

func NewClient(opts *ClientOptions) *Client {
	if opts.transport == nil {
		opts.transport = opts.HTTPClient
	}
	if opts.transport == nil {
		opts.transport = http.DefaultClient
	}

	if opts.BaseURL == "" {
		opts.BaseURL = "https://api.anthropic.com/v1"
	}

	return &Client{
		opts: opts,
	}
}

func (c *Client) NewResponses(ctx context.Context, inp *responses2.Request) (*responses2.Response, error) {
	anthropicRequest := anthropic_responses2.NativeRequestToRequest(inp)

	payload, err := sonic.Marshal(anthropicRequest)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.opts.BaseURL+"/messages", bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}

	req.Header.Set("x-api-key", c.opts.ApiKey)
	req.Header.Set("Anthropic-Version", "2023-06-01")

	var betaHeaders []string
	if anthropicRequest.OutputFormat != nil {
		betaHeaders = append(betaHeaders, "structured-outputs-2025-11-13")
	}
	for _, t := range anthropicRequest.Tools {
		if t.OfCodeExecutionTool != nil {
			betaHeaders = append(betaHeaders, "code-execution-2025-08-25")
		}
	}
	if betaHeaders != nil && len(betaHeaders) > 0 {
		req.Header.Set("anthropic-beta", strings.Join(betaHeaders, ","))
	}

	for k, v := range c.opts.Headers {
		req.Header.Set(k, v)
	}
	base.AddAdditionalHeaders(req, inp.ExtraFields)

	res, err := c.opts.transport.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	var anthropicResponse *anthropic_responses2.Response
	err = utils.DecodeJSON(res.Body, &anthropicResponse)
	if err != nil {
		return nil, err
	}

	if anthropicResponse.Error != nil {
		return nil, errors.New(anthropicResponse.Error.Message)
	}

	return anthropicResponse.ToNativeResponse(), nil
}

func (c *Client) NewStreamingResponses(ctx context.Context, inp *responses2.Request) (chan *responses2.ResponseChunk, error) {
	anthropicRequest := anthropic_responses2.NativeRequestToRequest(inp)

	payload, err := sonic.Marshal(anthropicRequest)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.opts.BaseURL+"/messages", bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.opts.ApiKey)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	var betaHeaders []string
	if anthropicRequest.OutputFormat != nil {
		betaHeaders = append(betaHeaders, "structured-outputs-2025-11-13")
	}
	for _, t := range anthropicRequest.Tools {
		if t.OfCodeExecutionTool != nil {
			betaHeaders = append(betaHeaders, "code-execution-2025-08-25")
		}
	}
	if betaHeaders != nil && len(betaHeaders) > 0 {
		req.Header.Set("anthropic-beta", strings.Join(betaHeaders, ","))
	}
	for k, v := range c.opts.Headers {
		req.Header.Set(k, v)
	}
	base.AddAdditionalHeaders(req, inp.ExtraFields)

	res, err := c.opts.transport.Do(req)
	if err != nil {
		return nil, err
	}

	if res.StatusCode != http.StatusOK {
		defer res.Body.Close()
		return nil, base.ParseErrorResponse(res)
	}

	converter := anthropic_responses2.ResponseChunkToNativeResponseChunkConverter{}
	return base.StreamResponsesSSE(ctx, res.Body, func(data []byte) ([]*responses2.ResponseChunk, error) {
		chunk := &anthropic_responses2.ResponseChunk{}
		if err := sonic.Unmarshal(data, chunk); err != nil {
			return nil, err
		}
		return converter.ResponseChunkToNativeResponseChunk(chunk), nil
	}), nil
}
