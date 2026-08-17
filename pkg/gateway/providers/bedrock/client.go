package bedrock

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/bytedance/sonic"
	responses2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/base"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/bedrock/bedrock_responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

type ClientOptions struct {
	// https://bedrock-runtime.us-east-1.amazonaws.com
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
		opts.BaseURL = "https://bedrock-runtime.us-east-1.amazonaws.com"
	}

	return &Client{
		opts: opts,
	}
}

// buildConverseURL constructs the Bedrock Converse URL for the given model.
// Format: {BaseURL}/model/{modelId}/converse
func buildConverseURL(baseURL, model string) string {
	return baseURL + "/model/" + url.PathEscape(model) + "/converse"
}

// buildConverseStreamURL constructs the Bedrock ConverseStream URL for the given model.
// Format: {BaseURL}/model/{modelId}/converse-stream
func buildConverseStreamURL(baseURL, model string) string {
	return baseURL + "/model/" + url.PathEscape(model) + "/converse-stream"
}

func (c *Client) NewResponses(ctx context.Context, inp *responses2.Request) (*responses2.Response, error) {
	converseReq := bedrock_responses.NativeRequestToConverseRequest(inp)

	payload, err := sonic.Marshal(converseReq)
	if err != nil {
		return nil, err
	}

	reqURL := buildConverseURL(c.opts.BaseURL, inp.Model)
	req, err := http.NewRequest(http.MethodPost, reqURL, bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.opts.ApiKey)
	base.AddAdditionalHeaders(req, inp.ExtraFields)

	// Apply custom headers (used for AWS auth headers like Authorization, x-amz-date, etc.)
	for k, v := range c.opts.Headers {
		req.Header.Set(k, v)
	}

	res, err := c.opts.transport.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		return nil, errors.New("bedrock converse failed (" + res.Status + "): " + string(body))
	}

	var converseResponse bedrock_responses.ConverseResponse
	err = utils.DecodeJSON(res.Body, &converseResponse)
	if err != nil {
		return nil, err
	}

	return converseResponse.ToNativeResponse(inp.Model), nil
}

func (c *Client) NewStreamingResponses(ctx context.Context, inp *responses2.Request) (chan *responses2.ResponseChunk, error) {
	converseReq := bedrock_responses.NativeRequestToConverseRequest(inp)

	payload, err := sonic.Marshal(converseReq)
	if err != nil {
		return nil, err
	}

	reqURL := buildConverseStreamURL(c.opts.BaseURL, inp.Model)
	req, err := http.NewRequest(http.MethodPost, reqURL, bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.amazon.eventstream")
	req.Header.Set("Authorization", "Bearer "+c.opts.ApiKey)
	base.AddAdditionalHeaders(req, inp.ExtraFields)

	// Apply custom headers (used for AWS auth headers)
	for k, v := range c.opts.Headers {
		req.Header.Set(k, v)
	}

	res, err := c.opts.transport.Do(req)
	if err != nil {
		return nil, err
	}

	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return nil, errors.New("bedrock converse-stream failed (" + res.Status + "): " + string(body))
	}

	// Bedrock signals some validation failures with HTTP 200 + JSON body instead
	// of an event-stream. Detect via Content-Type. The header is sometimes
	// empty/missing on success, so check positively for JSON rather than
	// negatively for the event-stream type.
	if ct := res.Header.Get("Content-Type"); strings.Contains(ct, "application/json") {
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		slog.WarnContext(ctx, "bedrock converse-stream rejected request",
			slog.String("content_type", ct),
			slog.String("response_body", string(body)),
			slog.String("request_payload", string(payload)),
		)
		return nil, errors.New("bedrock converse-stream returned JSON error (content-type=" + ct + "): " + string(body))
	}

	out := make(chan *responses2.ResponseChunk)

	go func() {
		defer res.Body.Close()
		defer close(out)

		converter := bedrock_responses.NewConverseStreamToNativeConverter(inp.Model)

		for {
			msg, err := decodeEventStreamMessage(res.Body)
			if err != nil {
				if err == io.EOF || err == io.ErrUnexpectedEOF {
					return
				}
				slog.WarnContext(ctx, "error decoding bedrock event stream message", slog.Any("error", err))
				return
			}

			// Check message type header
			messageType := msg.Headers[":message-type"]
			if messageType == "exception" {
				slog.WarnContext(ctx, "bedrock streaming exception",
					slog.String("exception-type", msg.Headers[":exception-type"]),
					slog.String("payload", string(msg.Payload)),
				)
				return
			}

			// Only process "event" messages
			if messageType != "event" {
				continue
			}

			// Use the event-type header to unmarshal payload into the correct struct
			eventType := msg.Headers[":event-type"]
			event, err := bedrock_responses.UnmarshalEventPayload(eventType, msg.Payload)
			if err != nil {
				slog.WarnContext(ctx, "error decoding converse stream event",
					slog.String("event-type", eventType),
					slog.Any("error", err),
				)
				continue
			}

			for _, nativeChunk := range converter.ConvertEvent(event) {
				out <- nativeChunk
			}
		}
	}()

	return out, nil
}
