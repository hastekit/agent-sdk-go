package openaicompat

import (
	"bufio"
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/bytedance/sonic"
	chat_completion2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/chat_completion"
	responses2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/base"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

type ClientOptions struct {
	// BaseURL is the API root, e.g. https://api.deepseek.com/v1 - the
	// chat completions path is appended to it.
	BaseURL string
	ApiKey  string
	Headers map[string]string

	// Authorize attaches credentials to the request. Defaults to
	// "Authorization: Bearer <ApiKey>", which is what every OpenAI-compatible
	// API accepts; providers with a different scheme override it.
	Authorize func(req *http.Request, apiKey string)

	Transport *http.Client
}

// Client talks to an OpenAI-compatible /chat/completions endpoint and
// presents it through the SDK's native Responses interface.
type Client struct {
	*base.BaseProvider
	opts *ClientOptions
}

func NewClient(opts *ClientOptions) *Client {
	if opts.Transport == nil {
		opts.Transport = http.DefaultClient
	}

	if opts.Authorize == nil {
		opts.Authorize = func(req *http.Request, apiKey string) {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
	}

	return &Client{opts: opts}
}

func (c *Client) BaseURL() string {
	return c.opts.BaseURL
}

func (c *Client) newRequest(ctx context.Context, path string, payload []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.opts.BaseURL+path, bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	c.opts.Authorize(req, c.opts.ApiKey)

	for k, v := range c.opts.Headers {
		req.Header.Set(k, v)
	}

	return req, nil
}

func (c *Client) NewResponses(ctx context.Context, in *responses2.Request) (*responses2.Response, error) {
	chatRequest := NativeRequestToChatRequest(in)
	chatRequest.Stream = utils.Ptr(false)
	chatRequest.StreamOptions = nil

	payload, err := sonic.Marshal(chatRequest)
	if err != nil {
		return nil, err
	}

	req, err := c.newRequest(ctx, "/chat/completions", payload)
	if err != nil {
		return nil, err
	}

	base.AddAdditionalHeaders(req, in.ExtraFields)

	res, err := c.opts.Transport.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, base.ParseErrorResponse(res)
	}

	var chatResponse *ChatResponse
	if err = utils.DecodeJSON(res.Body, &chatResponse); err != nil {
		return nil, err
	}

	if chatResponse.Error != nil && chatResponse.Error.Message != "" {
		return nil, chatResponse.Error
	}

	return chatResponse.ToNativeResponse(), nil
}

func (c *Client) NewStreamingResponses(ctx context.Context, in *responses2.Request) (chan *responses2.ResponseChunk, error) {
	chatRequest := NativeRequestToChatRequest(in)
	chatRequest.Stream = utils.Ptr(true)
	if chatRequest.StreamOptions == nil {
		chatRequest.StreamOptions = &StreamOptions{IncludeUsage: true}
	}

	payload, err := sonic.Marshal(chatRequest)
	if err != nil {
		return nil, err
	}

	req, err := c.newRequest(ctx, "/chat/completions", payload)
	if err != nil {
		return nil, err
	}

	base.AddAdditionalHeaders(req, in.ExtraFields)

	res, err := c.opts.Transport.Do(req)
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

		converter := NewStreamConverter()
		reader := bufio.NewReader(res.Body)

		emit := func(chunks []*responses2.ResponseChunk) bool {
			for _, chunk := range chunks {
				select {
				case out <- chunk:
				case <-ctx.Done():
					return false
				}
			}
			return true
		}

		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				break
			}

			line = strings.TrimRight(line, "\r\n")
			if !strings.HasPrefix(line, "data:") {
				continue
			}

			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				break
			}

			chunk := &ChatResponseChunk{}
			if err = sonic.Unmarshal([]byte(data), chunk); err != nil {
				slog.WarnContext(ctx, "unable to unmarshal chat completion chunk", slog.String("data", data), slog.Any("error", err))
				continue
			}

			if !emit(converter.Convert(chunk)) {
				return
			}
		}

		emit(converter.Finish())
	}()

	return out, nil
}

// NewChatCompletion passes the native chat completion request through
// unchanged - the native shape already is the OpenAI wire shape.
func (c *Client) NewChatCompletion(ctx context.Context, in *chat_completion2.Request) (*chat_completion2.Response, error) {
	payload, err := sonic.Marshal(in)
	if err != nil {
		return nil, err
	}

	req, err := c.newRequest(ctx, "/chat/completions", payload)
	if err != nil {
		return nil, err
	}

	res, err := c.opts.Transport.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, base.ParseErrorResponse(res)
	}

	var chatResponse *chat_completion2.Response
	if err = utils.DecodeJSON(res.Body, &chatResponse); err != nil {
		return nil, err
	}

	return chatResponse, nil
}

func (c *Client) NewStreamingChatCompletion(ctx context.Context, in *chat_completion2.Request) (chan *chat_completion2.ResponseChunk, error) {
	payload, err := sonic.Marshal(in)
	if err != nil {
		return nil, err
	}

	req, err := c.newRequest(ctx, "/chat/completions", payload)
	if err != nil {
		return nil, err
	}

	res, err := c.opts.Transport.Do(req)
	if err != nil {
		return nil, err
	}

	if res.StatusCode != http.StatusOK {
		defer res.Body.Close()
		return nil, base.ParseErrorResponse(res)
	}

	out := make(chan *chat_completion2.ResponseChunk)

	go func() {
		defer res.Body.Close()
		defer close(out)

		reader := bufio.NewReader(res.Body)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}

			line = strings.TrimRight(line, "\r\n")
			if !strings.HasPrefix(line, "data:") {
				continue
			}

			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				return
			}

			chunk := &chat_completion2.ResponseChunk{}
			if err = sonic.Unmarshal([]byte(data), chunk); err != nil {
				slog.WarnContext(ctx, "unable to unmarshal chat completion chunk", slog.String("data", data), slog.Any("error", err))
				continue
			}

			select {
			case out <- chunk:
			case <-ctx.Done():
				return
			}
		}
	}()

	return out, nil
}

func (e *ChatError) Error() string {
	if e.Code != "" {
		return e.Message + " (" + e.Code + ")"
	}

	return e.Message
}
