package gemini

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/bytedance/sonic"
	embeddings2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/embeddings"
	image_edit2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/image_edit"
	image_generation2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/image_generation"
	responses2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	speech2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/speech"
	transcription2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/transcription"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/base"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/gemini/gemini_embeddings"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/gemini/gemini_image_edit"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/gemini/gemini_image_generation"
	gemini_responses2 "github.com/hastekit/agent-sdk-go/pkg/gateway/providers/gemini/gemini_responses"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/gemini/gemini_speech"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/gemini/gemini_transcription"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

type ClientOptions struct {
	// https://generativelanguage.googleapis.com/v1beta
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
		opts.BaseURL = "https://generativelanguage.googleapis.com/v1beta"
	}

	return &Client{
		opts: opts,
	}
}

func (c *Client) NewResponses(ctx context.Context, inp *responses2.Request) (*responses2.Response, error) {
	in := gemini_responses2.ResponsesInputToGeminiResponsesInput(inp)

	// Construct the API endpoint
	// Format: https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent
	model := inp.Model
	if model == "" {
		model = "gemini-2-5-flash"
	}
	endpoint := fmt.Sprintf("%s/models/%s:generateContent", c.opts.BaseURL, model)

	payload, err := sonic.Marshal(in)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	if c.opts.ApiKey != "" {
		// Gemini API uses query parameter for API key
		q := req.URL.Query()
		q.Set("key", c.opts.ApiKey)
		req.URL.RawQuery = q.Encode()
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

	var geminiResponse *gemini_responses2.Response
	err = utils.DecodeJSON(res.Body, &geminiResponse)
	if err != nil {
		return nil, err
	}

	if geminiResponse.Error != nil {
		return nil, fmt.Errorf("gemini API error: %s (code: %d, status: %s)", geminiResponse.Error.Message, geminiResponse.Error.Code, geminiResponse.Error.Status)
	}

	return geminiResponse.ToNativeResponse(), nil
}

func (c *Client) NewStreamingResponses(ctx context.Context, inp *responses2.Request) (chan *responses2.ResponseChunk, error) {
	in := gemini_responses2.ResponsesInputToGeminiResponsesInput(inp)

	// Construct the API endpoint for streaming
	model := inp.Model
	if model == "" {
		model = "gemini-2.5-flash"
	}
	endpoint := fmt.Sprintf("%s/models/%s:streamGenerateContent", c.opts.BaseURL, model)

	payload, err := sonic.Marshal(in)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", c.opts.ApiKey)
	base.AddAdditionalHeaders(req, inp.ExtraFields)

	for k, v := range c.opts.Headers {
		req.Header.Set(k, v)
	}

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
		converter := gemini_responses2.ResponseChunkToNativeResponseChunkConverter{}

		var data strings.Builder
		inQuotes := false
		escaping := false
		openBracesCount := 0
		for {

			line, err := reader.ReadString('\n')
			for _, ch := range line {
				if ch == '{' && !inQuotes {
					openBracesCount++
				}

				// If object has not started, discard the character
				// This is skip the initial `[` and last `]` and `,` between the objects
				if openBracesCount == 0 {
					continue
				}

				// Accumulate all the other characters
				data.WriteByte(byte(ch))

				// Double quotes
				if ch == '"' && !escaping {
					inQuotes = !inQuotes
					continue
				}

				// Backslash
				escaping = ch == 92

				// If closing bracket, then check for end of the chunk
				if ch == '}' && !inQuotes {
					openBracesCount--
					if openBracesCount == 0 {
						geminiChunk := &gemini_responses2.Response{}
						err = sonic.Unmarshal([]byte(data.String()), &geminiChunk)
						if err == nil {
							//fmt.Println("---\nGemini chunk -> " + strings.TrimPrefix(data.String(), "data:"))
							for _, nativeChunk := range converter.ResponseChunkToNativeResponseChunk(geminiChunk) {
								//d, _ := sonic.Marshal(nativeChunk)
								//fmt.Println("\t\t <- Native Chunk" + string(d))
								out <- nativeChunk
							}
						}

						data.Reset()
					}
				}
			}

			if err != nil {
				for _, nativeChunk := range converter.ResponseChunkToNativeResponseChunk(nil) {
					//d, _ := sonic.Marshal(nativeChunk)
					//fmt.Println("\t\t <- Native Chunk" + string(d))
					out <- nativeChunk
				}
				return
			}
		}
	}()

	return out, nil
}

func (c *Client) NewEmbedding(ctx context.Context, inp *embeddings2.Request) (*embeddings2.Response, error) {
	geminiRequest := gemini_embeddings.NativeRequestToRequest(inp)

	model := inp.Model
	if model == "" {
		model = "models/gemini-embedding-001"
	}

	action := "embedContent"
	if len(geminiRequest.Requests) > 0 {
		action = "batchEmbedContents"
	}

	endpoint := fmt.Sprintf("%s/%s:%s", c.opts.BaseURL, model, action)

	payload, err := sonic.Marshal(geminiRequest)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", c.opts.ApiKey)

	for k, v := range c.opts.Headers {
		req.Header.Set(k, v)
	}

	res, err := c.opts.transport.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, base.ParseErrorResponse(res)
	}

	var geminiResponse *gemini_embeddings.Response
	err = utils.DecodeJSON(res.Body, &geminiResponse)
	if err != nil {
		return nil, err
	}

	return geminiResponse.ToNativeResponse(model), nil
}

func (c *Client) NewSpeech(ctx context.Context, inp *speech2.Request) (*speech2.Response, error) {
	geminiRequest := gemini_speech.NativeRequestToRequest(inp)

	model := inp.Model
	if model == "" {
		model = "gemini-2.5-flash-preview-tts"
	}

	action := "generateContent"
	endpoint := fmt.Sprintf("%s/%s:%s", c.opts.BaseURL, "models/"+model, action)

	payload, err := sonic.Marshal(geminiRequest)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", c.opts.ApiKey)

	for k, v := range c.opts.Headers {
		req.Header.Set(k, v)
	}

	res, err := c.opts.transport.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, base.ParseErrorResponse(res)
	}

	var geminiResponse *gemini_speech.Response
	err = utils.DecodeJSON(res.Body, &geminiResponse)
	if err != nil {
		return nil, err
	}

	return geminiResponse.ToNativeResponse(inp.ResponseFormat), nil
}

func (c *Client) NewStreamingSpeech(ctx context.Context, inp *speech2.Request) (chan *speech2.ResponseChunk, error) {
	geminiRequest := gemini_speech.NativeRequestToRequest(inp)

	model := inp.Model
	if model == "" {
		model = "gemini-2.5-flash-preview-tts"
	}

	action := "streamGenerateContent"
	endpoint := fmt.Sprintf("%s/%s:%s", c.opts.BaseURL, "models/"+model, action)

	payload, err := sonic.Marshal(geminiRequest)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", c.opts.ApiKey)

	for k, v := range c.opts.Headers {
		req.Header.Set(k, v)
	}

	res, err := c.opts.transport.Do(req)
	if err != nil {
		return nil, err
	}

	if res.StatusCode != http.StatusOK {
		defer res.Body.Close()
		return nil, base.ParseErrorResponse(res)
	}

	out := make(chan *speech2.ResponseChunk)

	go func() {
		defer res.Body.Close()
		defer close(out)

		reader := bufio.NewReader(res.Body)
		converter := gemini_speech.ResponseChunkToNativeResponseChunkConverter{}

		var data strings.Builder
		inQuotes := false
		escaping := false
		openBracesCount := 0
		for {

			line, err := reader.ReadString('\n')
			for _, ch := range line {
				if ch == '{' && !inQuotes {
					openBracesCount++
				}

				// If object has not started, discard the character
				// This is skip the initial `[` and last `]` and `,` between the objects
				if openBracesCount == 0 {
					continue
				}

				// Accumulate all the other characters
				data.WriteByte(byte(ch))

				// Double quotes
				if ch == '"' && !escaping {
					inQuotes = !inQuotes
					continue
				}

				// Backslash
				escaping = ch == 92

				// If closing bracket, then check for end of the chunk
				if ch == '}' && !inQuotes {
					openBracesCount--
					if openBracesCount == 0 {
						geminiChunk := &gemini_speech.Response{}
						err = sonic.Unmarshal([]byte(data.String()), &geminiChunk)
						if err == nil {
							//fmt.Println("---\nGemini chunk -> " + strings.TrimPrefix(data.String(), "data:"))
							for _, nativeChunk := range converter.ResponseChunkToNativeResponseChunk(geminiChunk) {
								//d, _ := sonic.Marshal(nativeChunk)
								//fmt.Println("\t\t <- Native Chunk" + string(d))
								out <- nativeChunk
							}
						}

						data.Reset()
					}
				}
			}

			if err != nil {
				for _, nativeChunk := range converter.ResponseChunkToNativeResponseChunk(nil) {
					out <- nativeChunk
				}
				return
			}
		}
	}()

	return out, nil
}

func (c *Client) NewTranscription(ctx context.Context, inp *transcription2.Request) (*transcription2.Response, error) {
	geminiRequest := gemini_transcription.NativeRequestToRequest(inp)

	model := inp.Model
	if model == "" {
		model = "gemini-2.0-flash"
	}

	endpoint := fmt.Sprintf("%s/models/%s:generateContent", c.opts.BaseURL, model)

	payload, err := sonic.Marshal(geminiRequest)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", c.opts.ApiKey)

	for k, v := range c.opts.Headers {
		req.Header.Set(k, v)
	}

	res, err := c.opts.transport.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, base.ParseErrorResponse(res)
	}

	var geminiResponse *gemini_transcription.Response
	err = utils.DecodeJSON(res.Body, &geminiResponse)
	if err != nil {
		return nil, err
	}

	return geminiResponse.ToNativeResponse(), nil
}

func (c *Client) NewImageGeneration(ctx context.Context, inp *image_generation2.Request) (*image_generation2.Response, error) {
	geminiRequest := gemini_image_generation.NativeRequestToRequest(inp)

	model := inp.Model
	if model == "" {
		model = "gemini-2.5-flash-preview-image"
	}

	endpoint := fmt.Sprintf("%s/models/%s:generateContent", c.opts.BaseURL, model)

	payload, err := sonic.Marshal(geminiRequest)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", c.opts.ApiKey)

	for k, v := range c.opts.Headers {
		req.Header.Set(k, v)
	}

	res, err := c.opts.transport.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		var errResp map[string]any
		err = utils.DecodeJSON(res.Body, &errResp)
		if err != nil {
			return nil, err
		}
		if errorObj, ok := errResp["error"].(map[string]any); ok {
			if message, ok := errorObj["message"].(string); ok {
				return nil, fmt.Errorf("gemini API error: %s", message)
			}
		}
		return nil, errors.New("unknown error occurred")
	}

	var geminiResponse *gemini_image_generation.Response
	err = utils.DecodeJSON(res.Body, &geminiResponse)
	if err != nil {
		return nil, err
	}

	if geminiResponse.Error != nil {
		return nil, fmt.Errorf("gemini API error: %s (code: %d, status: %s)", geminiResponse.Error.Message, geminiResponse.Error.Code, geminiResponse.Error.Status)
	}

	return geminiResponse.ToNativeResponse(), nil
}

func (c *Client) NewImageEdit(ctx context.Context, inp *image_edit2.Request) (*image_edit2.Response, error) {
	geminiRequest := gemini_image_edit.NativeRequestToRequest(inp)

	model := inp.Model
	if model == "" {
		model = "gemini-2.5-flash-preview-image"
	}

	endpoint := fmt.Sprintf("%s/models/%s:generateContent", c.opts.BaseURL, model)

	payload, err := sonic.Marshal(geminiRequest)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", c.opts.ApiKey)

	for k, v := range c.opts.Headers {
		req.Header.Set(k, v)
	}

	res, err := c.opts.transport.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		var errResp map[string]any
		err = utils.DecodeJSON(res.Body, &errResp)
		if err != nil {
			return nil, err
		}
		if errorObj, ok := errResp["error"].(map[string]any); ok {
			if message, ok := errorObj["message"].(string); ok {
				return nil, fmt.Errorf("gemini API error: %s", message)
			}
		}
		return nil, errors.New("unknown error occurred")
	}

	var geminiEditResponse *gemini_image_edit.Response
	err = utils.DecodeJSON(res.Body, &geminiEditResponse)
	if err != nil {
		return nil, err
	}

	if geminiEditResponse.Error != nil {
		return nil, fmt.Errorf("gemini API error: %s (code: %d, status: %s)", geminiEditResponse.Error.Message, geminiEditResponse.Error.Code, geminiEditResponse.Error.Status)
	}

	return geminiEditResponse.ToNativeResponse(), nil
}
