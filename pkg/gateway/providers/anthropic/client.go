package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"log/slog"
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

	transport *http.Client
}

type Client struct {
	*base.BaseProvider
	opts *ClientOptions
}

func NewClient(opts *ClientOptions) *Client {
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

	req, err := http.NewRequest(http.MethodPost, c.opts.BaseURL+"/messages", bytes.NewBuffer(payload))
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

	req, err := http.NewRequest(http.MethodPost, c.opts.BaseURL+"/messages", bytes.NewBuffer(payload))
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

	out := make(chan *responses2.ResponseChunk)

	go func() {
		defer res.Body.Close()
		defer close(out)

		reader := bufio.NewReader(res.Body)
		converter := anthropic_responses2.ResponseChunkToNativeResponseChunkConverter{}

		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}

			line = strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(line, "data:") {
				anthropicResponseChunk := &anthropic_responses2.ResponseChunk{}
				if err = sonic.Unmarshal([]byte(strings.TrimPrefix(line, "data:")), anthropicResponseChunk); err != nil {
					slog.WarnContext(ctx, "unable to unmarshal anthropic response chunk", slog.String("data", line), slog.Any("error", err))
					continue
				}

				//fmt.Println("---\nAnthropic chunk -> " + strings.TrimPrefix(line, "data:"))
				for _, nativeChunk := range converter.ResponseChunkToNativeResponseChunk(anthropicResponseChunk) {
					//d, _ := sonic.Marshal(nativeChunk)
					//fmt.Println("\t\t <- Native Chunk" + string(d))
					out <- nativeChunk
				}
			}
		}
	}()

	return out, nil
}
