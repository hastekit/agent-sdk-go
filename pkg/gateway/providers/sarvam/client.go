// Package sarvam is the provider client for Sarvam AI - chat (the
// OpenAI-compatible /v1/chat/completions endpoint, bridged to the native
// Responses shape), text-to-speech and speech-to-text.
package sarvam

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"

	"github.com/bytedance/sonic"
	chat_completion2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/chat_completion"
	responses2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	speech2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/speech"
	transcription2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/transcription"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/base"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/openaicompat"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/sarvam/sarvam_speech"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/sarvam/sarvam_transcription"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

// DefaultBaseURL is the Sarvam API root. Speech endpoints hang directly off
// it; chat lives under /v1.
const DefaultBaseURL = "https://api.sarvam.ai"

// APIKeyHeader is the header Sarvam authenticates every endpoint with. The
// chat endpoint additionally accepts an Authorization bearer token, and the
// client sends both.
const APIKeyHeader = "api-subscription-key"

type ClientOptions struct {
	// https://api.sarvam.ai
	BaseURL string
	ApiKey  string
	Headers map[string]string

	transport *http.Client
}

type Client struct {
	*base.BaseProvider
	// chat handles everything on the OpenAI-compatible surface; Sarvam has
	// no /responses endpoint, so the native Responses calls are bridged onto
	// /v1/chat/completions.
	chat *openaicompat.Client
	opts *ClientOptions
}

func NewClient(opts *ClientOptions) *Client {
	if opts.transport == nil {
		opts.transport = http.DefaultClient
	}

	if opts.BaseURL == "" {
		opts.BaseURL = DefaultBaseURL
	}

	return &Client{
		opts: opts,
		chat: openaicompat.NewClient(&openaicompat.ClientOptions{
			BaseURL: opts.BaseURL + "/v1",
			ApiKey:  opts.ApiKey,
			Headers: opts.Headers,
			Authorize: func(req *http.Request, apiKey string) {
				req.Header.Set("Authorization", "Bearer "+apiKey)
				req.Header.Set(APIKeyHeader, apiKey)
			},
			Transport: opts.transport,
		}),
	}
}

func (c *Client) NewResponses(ctx context.Context, in *responses2.Request) (*responses2.Response, error) {
	return c.chat.NewResponses(ctx, in)
}

func (c *Client) NewStreamingResponses(ctx context.Context, in *responses2.Request) (chan *responses2.ResponseChunk, error) {
	return c.chat.NewStreamingResponses(ctx, in)
}

func (c *Client) NewChatCompletion(ctx context.Context, in *chat_completion2.Request) (*chat_completion2.Response, error) {
	return c.chat.NewChatCompletion(ctx, in)
}

func (c *Client) NewStreamingChatCompletion(ctx context.Context, in *chat_completion2.Request) (chan *chat_completion2.ResponseChunk, error) {
	return c.chat.NewStreamingChatCompletion(ctx, in)
}

func (c *Client) NewSpeech(ctx context.Context, in *speech2.Request) (*speech2.Response, error) {
	sarvamRequest := sarvam_speech.NativeRequestToRequest(in)

	payload, err := sonic.Marshal(sarvamRequest)
	if err != nil {
		return nil, err
	}

	req, err := c.newRequest(ctx, "/text-to-speech", bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")

	res, err := c.opts.transport.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, base.ParseErrorResponse(res)
	}

	var sarvamResponse *sarvam_speech.Response
	if err = utils.DecodeJSON(res.Body, &sarvamResponse); err != nil {
		return nil, err
	}

	return sarvamResponse.ToNativeResponse(sarvamRequest.OutputAudioCodec), nil
}

// NewStreamingSpeech synthesizes in one shot and delivers the result as a
// single audio delta. Sarvam's incremental TTS is a WebSocket API, not an
// HTTP stream, so this keeps the streaming interface usable without
// pretending to stream.
func (c *Client) NewStreamingSpeech(ctx context.Context, in *speech2.Request) (chan *speech2.ResponseChunk, error) {
	res, err := c.NewSpeech(ctx, in)
	if err != nil {
		return nil, err
	}

	out := make(chan *speech2.ResponseChunk, 2)
	out <- &speech2.ResponseChunk{
		OfAudioDelta: &speech2.ChunkAudioDelta[speech2.ChunkTypeAudioDelta]{
			Audio: string(res.Audio),
		},
	}
	out <- &speech2.ResponseChunk{
		OfAudioDone: &speech2.ChunkAudioDone[speech2.ChunkTypeAudioDone]{
			Usage: res.Usage,
		},
	}
	close(out)

	return out, nil
}

func (c *Client) NewTranscription(ctx context.Context, in *transcription2.Request) (*transcription2.Response, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	filename := in.AudioFilename
	if filename == "" {
		filename = "audio.wav"
	}

	filePart, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return nil, err
	}
	if _, err = filePart.Write(in.Audio); err != nil {
		return nil, err
	}

	// Model is left to the API default when unset - the accepted enum moves
	// with the model generation, and an unknown value is a hard error.
	if in.Model != "" {
		if err = writer.WriteField("model", in.Model); err != nil {
			return nil, err
		}
	}

	languageCode := sarvam_transcription.LanguageCodeUnknown
	if in.Language != nil && *in.Language != "" {
		languageCode = *in.Language
	}
	if err = writer.WriteField("language_code", languageCode); err != nil {
		return nil, err
	}

	if len(in.TimestampGranularities) > 0 {
		if err = writer.WriteField("with_timestamps", "true"); err != nil {
			return nil, err
		}
	}

	if err = writer.Close(); err != nil {
		return nil, err
	}

	req, err := c.newRequest(ctx, "/speech-to-text", &buf)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())

	res, err := c.opts.transport.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, base.ParseErrorResponse(res)
	}

	var sarvamResponse *sarvam_transcription.Response
	if err = utils.DecodeJSON(res.Body, &sarvamResponse); err != nil {
		return nil, err
	}

	return sarvamResponse.ToNativeResponse(), nil
}

func (c *Client) newRequest(ctx context.Context, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.opts.BaseURL+path, body)
	if err != nil {
		return nil, err
	}

	req.Header.Set(APIKeyHeader, c.opts.ApiKey)

	for k, v := range c.opts.Headers {
		req.Header.Set(k, v)
	}

	return req, nil
}
