package bedrock

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewBuffer(payload))
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewBuffer(payload))
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

	// The event stream is binary rather than SSE, so it cannot share
	// base.StreamResponsesSSE, but it follows the same contract: a terminal
	// failure reaches the caller as a chunk, undecodable events are skipped,
	// and only the metadata event marks the response complete.
	go func() {
		defer close(out)
		defer res.Body.Close()
		stop := context.AfterFunc(ctx, func() { _ = res.Body.Close() })
		defer stop()
		fail := func(err error) { base.SendResponseChunk(ctx, out, responses2.NewStreamError(err)) }

		converter := bedrock_responses.NewConverseStreamToNativeConverter(inp.Model)

		for {
			msg, err := decodeEventStreamMessage(res.Body)
			if err != nil {
				// The converter completes the response on the terminal
				// metadata event, and that path has already returned by now.
				// Reaching the end of the body here means the stream stopped
				// early, which EOF alone cannot distinguish from success.
				if err == io.EOF {
					err = io.ErrUnexpectedEOF
				}
				fail(err)
				return
			}

			switch msg.Headers[":message-type"] {
			case "exception":
				fail(bedrockStreamException(msg))
				return
			case "event":
			default:
				continue
			}

			// Use the event-type header to unmarshal payload into the correct struct
			eventType := msg.Headers[":event-type"]
			event, err := bedrock_responses.UnmarshalEventPayload(eventType, msg.Payload)
			if err != nil {
				// Bedrock adds event types independently of SDK releases. An
				// event this build cannot decode is not evidence that the
				// stream failed; keep reading for its terminal event.
				slog.WarnContext(ctx, "skipping unrecognized converse stream event",
					slog.String("event-type", eventType),
					slog.Any("error", err),
				)
				continue
			}

			for _, nativeChunk := range converter.ConvertEvent(event) {
				if nativeChunk == nil {
					continue
				}
				if !base.SendResponseChunk(ctx, out, nativeChunk) {
					return
				}
				if nativeChunk.OfResponseCompleted != nil || nativeChunk.OfError != nil {
					return
				}
			}
		}
	}()

	return out, nil
}

// bedrockStreamException renders an event-stream exception message. Its payload
// is JSON carrying a "message" field; fall back to the raw bytes when a new
// exception shape does not match.
func bedrockStreamException(msg *eventStreamMessage) error {
	kind := msg.Headers[":exception-type"]
	if kind == "" {
		kind = "exception"
	}
	var payload struct {
		Message string `json:"message"`
	}
	if err := sonic.Unmarshal(msg.Payload, &payload); err == nil && payload.Message != "" {
		return fmt.Errorf("bedrock %s: %s", kind, payload.Message)
	}
	return fmt.Errorf("bedrock %s: %s", kind, string(msg.Payload))
}
