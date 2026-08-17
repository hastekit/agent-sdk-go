package openai

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bytedance/sonic"
	chat_completion2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/chat_completion"
	embeddings2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/embeddings"
	image_edit2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/image_edit"
	image_generation2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/image_generation"
	responses2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	speech2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/speech"
	transcription2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/transcription"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/base"
	openai_chat_completion2 "github.com/hastekit/agent-sdk-go/pkg/gateway/providers/openai/openai_chat_completion"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/openai/openai_embeddings"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/openai/openai_image_edit"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/openai/openai_image_generation"
	openai_responses2 "github.com/hastekit/agent-sdk-go/pkg/gateway/providers/openai/openai_responses"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/openai/openai_speech"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/openai/openai_transcription"
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
		opts.BaseURL = "https://api.openai.com/v1"
	}

	return &Client{
		opts: opts,
	}
}

func (c *Client) NewResponses(ctx context.Context, inp *responses2.Request) (*responses2.Response, error) {
	openAiRequest := openai_responses2.NativeRequestToRequest(inp)

	payload, err := sonic.Marshal(openAiRequest)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, c.opts.BaseURL+"/responses", bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+c.opts.ApiKey)
	base.AddAdditionalHeaders(req, inp.ExtraFields)

	res, err := c.opts.transport.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	var openAiResponse *openai_responses2.Response
	err = utils.DecodeJSON(res.Body, &openAiResponse)
	if err != nil {
		return nil, err
	}

	if openAiResponse.Error != nil {
		return nil, errors.New(openAiResponse.Error.Message)
	}

	return openAiResponse.ToNativeResponse(), nil
}

func (c *Client) NewStreamingResponses(ctx context.Context, inp *responses2.Request) (chan *responses2.ResponseChunk, error) {
	openAiRequest := openai_responses2.NativeRequestToRequest(inp)

	payload, err := sonic.Marshal(openAiRequest)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, c.opts.BaseURL+"/responses", bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.opts.ApiKey)
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

		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}

			line = strings.TrimRight(line, "\r\n")
			//fmt.Println(line)
			if strings.HasPrefix(line, "data:") {
				openAiResponseChunk := &openai_responses2.ResponseChunk{}
				err = sonic.Unmarshal([]byte(strings.TrimPrefix(line, "data:")), openAiResponseChunk)
				if err != nil {
					slog.WarnContext(ctx, "unable to unmarshal openai response chunk", slog.String("data", line), slog.Any("error", err))
					continue
				}
				out <- openAiResponseChunk.ToNativeResponseChunk()
			}
		}
	}()

	return out, nil
}

func (c *Client) NewEmbedding(ctx context.Context, inp *embeddings2.Request) (*embeddings2.Response, error) {
	openAiRequest := openai_embeddings.NativeRequestToRequest(inp)

	payload, err := sonic.Marshal(openAiRequest)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, c.opts.BaseURL+"/embeddings", bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.opts.ApiKey)

	res, err := c.opts.transport.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, base.ParseErrorResponse(res)
	}

	var openAiResponse *openai_embeddings.Response
	err = utils.DecodeJSON(res.Body, &openAiResponse)
	if err != nil {
		return nil, err
	}

	return openAiResponse.ToNativeResponse(), nil
}

func (c *Client) NewChatCompletion(ctx context.Context, inp *chat_completion2.Request) (*chat_completion2.Response, error) {
	openAiRequest := openai_chat_completion2.NativeRequestToRequest(inp)

	payload, err := sonic.Marshal(openAiRequest)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, c.opts.BaseURL+"/chat/completions", bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.opts.ApiKey)

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
				return nil, errors.New(message)
			}
		}
		return nil, errors.New("unknown error occurred")
	}

	var openAiResponse *openai_chat_completion2.Response
	err = utils.DecodeJSON(res.Body, &openAiResponse)
	if err != nil {
		return nil, err
	}

	return openAiResponse.ToNativeResponse(), nil
}

func (c *Client) NewStreamingChatCompletion(ctx context.Context, inp *chat_completion2.Request) (chan *chat_completion2.ResponseChunk, error) {
	openAiRequest := openai_chat_completion2.NativeRequestToRequest(inp)

	payload, err := sonic.Marshal(openAiRequest)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, c.opts.BaseURL+"/chat/completions", bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.opts.ApiKey)

	res, err := c.opts.transport.Do(req)
	if err != nil {
		return nil, err
	}

	if res.StatusCode != http.StatusOK {
		var errResp map[string]any
		err = utils.DecodeJSON(res.Body, &errResp)
		if err != nil {
			return nil, err
		}
		if errorObj, ok := errResp["error"].(map[string]any); ok {
			if message, ok := errorObj["message"].(string); ok {
				return nil, errors.New(message)
			}
		}
		return nil, errors.New("unknown error occurred")
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

			if line == "data: [DONE]" {
				return
			}

			if strings.HasPrefix(line, "data:") {
				openAiChatCompletionChunk := &openai_chat_completion2.ResponseChunk{}
				err = sonic.Unmarshal([]byte(strings.TrimPrefix(line, "data:")), openAiChatCompletionChunk)
				if err != nil {
					slog.WarnContext(ctx, "unable to unmarshal chat completion response chunk", slog.String("data", line), slog.Any("error", err))
					continue
				}
				out <- openAiChatCompletionChunk.ToNativeResponseChunk()
			}
		}
	}()

	return out, nil
}

func (c *Client) NewSpeech(ctx context.Context, in *speech2.Request) (*speech2.Response, error) {
	openAiRequest := openai_speech.NativeRequestToRequest(in)

	payload, err := sonic.Marshal(openAiRequest)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, c.opts.BaseURL+"/audio/speech", bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.opts.ApiKey)

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
				return nil, errors.New(message)
			}
		}
		return nil, errors.New("unknown error occurred")
	}

	// Handle gzip compressed response
	var reader io.Reader = res.Body
	if res.Header.Get("Content-Encoding") == "gzip" {
		gzipReader, err := gzip.NewReader(res.Body)
		if err != nil {
			return nil, err
		}
		defer gzipReader.Close()
		reader = gzipReader
	}

	// Read the raw audio binary data (decompressed if gzip)
	audioData, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	// Create response with audio data
	openAiResponse := &openai_speech.Response{
		Response: speech2.Response{
			Audio:       audioData,
			ContentType: res.Header.Get("Content-Type"),
		},
	}

	return openAiResponse.ToNativeResponse(), nil
}

func (c *Client) NewStreamingSpeech(ctx context.Context, in *speech2.Request) (chan *speech2.ResponseChunk, error) {
	openAiRequest := openai_speech.NativeRequestToRequest(in)

	payload, err := sonic.Marshal(openAiRequest)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, c.opts.BaseURL+"/audio/speech", bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.opts.ApiKey)

	res, err := c.opts.transport.Do(req)
	if err != nil {
		return nil, err
	}

	if res.StatusCode != http.StatusOK {
		var errResp map[string]any
		err = utils.DecodeJSON(res.Body, &errResp)
		if err != nil {
			return nil, err
		}
		if errorObj, ok := errResp["error"].(map[string]any); ok {
			if message, ok := errorObj["message"].(string); ok {
				return nil, errors.New(message)
			}
		}
		return nil, errors.New("unknown error occurred")
	}

	out := make(chan *speech2.ResponseChunk)

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

			if line == "data: [DONE]" {
				return
			}

			if strings.HasPrefix(line, "data:") {
				openAiSpeechChunk := &openai_speech.ResponseChunk{}
				err = sonic.Unmarshal([]byte(strings.TrimPrefix(line, "data:")), openAiSpeechChunk)
				if err != nil {
					slog.WarnContext(ctx, "unable to unmarshal speech response chunk", slog.String("data", line), slog.Any("error", err))
					continue
				}
				out <- openAiSpeechChunk.ToNativeResponse()
			}
		}
	}()

	return out, nil
}

func (c *Client) NewTranscription(ctx context.Context, in *transcription2.Request) (*transcription2.Response, error) {
	_ = openai_transcription.NativeRequestToRequest(in)

	// Build multipart form body
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// Add the audio file
	filename := in.AudioFilename
	if filename == "" {
		filename = "audio.mp3"
	}
	filePart, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return nil, err
	}
	if _, err = filePart.Write(in.Audio); err != nil {
		return nil, err
	}

	// Add model field
	if err = writer.WriteField("model", in.Model); err != nil {
		return nil, err
	}

	// Add optional fields
	if in.Language != nil {
		if err = writer.WriteField("language", *in.Language); err != nil {
			return nil, err
		}
	}

	if in.Prompt != nil {
		if err = writer.WriteField("prompt", *in.Prompt); err != nil {
			return nil, err
		}
	}

	if in.ResponseFormat != nil {
		if err = writer.WriteField("response_format", *in.ResponseFormat); err != nil {
			return nil, err
		}
	}

	if in.Temperature != nil {
		if err = writer.WriteField("temperature", strconv.FormatFloat(*in.Temperature, 'f', -1, 64)); err != nil {
			return nil, err
		}
	}

	for _, g := range in.TimestampGranularities {
		if err = writer.WriteField("timestamp_granularities[]", g); err != nil {
			return nil, err
		}
	}

	if err = writer.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, c.opts.BaseURL+"/audio/transcriptions", &buf)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.opts.ApiKey)

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
				return nil, errors.New(message)
			}
		}
		return nil, errors.New("unknown error occurred")
	}

	var openAiResponse *openai_transcription.Response
	err = utils.DecodeJSON(res.Body, &openAiResponse)
	if err != nil {
		return nil, err
	}

	return openAiResponse.ToNativeResponse(), nil
}

func (c *Client) NewImageGeneration(ctx context.Context, in *image_generation2.Request) (*image_generation2.Response, error) {
	openAiRequest := openai_image_generation.NativeRequestToRequest(in)

	payload, err := sonic.Marshal(openAiRequest)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, c.opts.BaseURL+"/images/generations", bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.opts.ApiKey)

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
				return nil, errors.New(message)
			}
		}
		return nil, errors.New("unknown error occurred")
	}

	var openAiResponse *openai_image_generation.Response
	err = utils.DecodeJSON(res.Body, &openAiResponse)
	if err != nil {
		return nil, err
	}

	if openAiResponse.Error != nil {
		return nil, errors.New(openAiResponse.Error.Message)
	}

	return openAiResponse.ToNativeResponse(), nil
}

func (c *Client) NewImageEdit(ctx context.Context, in *image_edit2.Request) (*image_edit2.Response, error) {
	// OpenAI image edit uses multipart/form-data with image[] for multiple images
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// Add images using image[] field name (supports up to 16 images for GPT image models)
	// Use CreatePart instead of CreateFormFile to set the correct Content-Type per image
	for i, img := range in.Images {
		filename := img.Filename
		if filename == "" {
			filename = fmt.Sprintf("image_%d.png", i)
		}

		contentType := "application/octet-stream"
		if ext := filepath.Ext(filename); ext != "" {
			if detected := mime.TypeByExtension(ext); detected != "" {
				contentType = detected
			}
		}

		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="image[]"; filename="%s"`, filename))
		header.Set("Content-Type", contentType)

		filePart, err := writer.CreatePart(header)
		if err != nil {
			return nil, err
		}
		if _, err = filePart.Write(img.Data); err != nil {
			return nil, err
		}
	}

	// Add prompt
	if err := writer.WriteField("prompt", in.Prompt); err != nil {
		return nil, err
	}

	// Add model
	if in.Model != "" {
		if err := writer.WriteField("model", in.Model); err != nil {
			return nil, err
		}
	}

	// Add optional fields
	if in.N != nil {
		if err := writer.WriteField("n", strconv.Itoa(*in.N)); err != nil {
			return nil, err
		}
	}

	if in.Size != nil {
		if err := writer.WriteField("size", *in.Size); err != nil {
			return nil, err
		}
	}

	if in.Quality != nil {
		if err := writer.WriteField("quality", *in.Quality); err != nil {
			return nil, err
		}
	}

	// Response format is only supported for DALL-E 2
	if in.ResponseFormat != nil && in.Model == "dall-e-2" {
		if err := writer.WriteField("response_format", *in.ResponseFormat); err != nil {
			return nil, err
		}
	}

	if in.OutputFormat != nil {
		if err := writer.WriteField("output_format", *in.OutputFormat); err != nil {
			return nil, err
		}
	}

	if err := writer.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, c.opts.BaseURL+"/images/edits", &buf)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.opts.ApiKey)

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
				return nil, errors.New(message)
			}
		}
		return nil, errors.New("unknown error occurred")
	}

	var openAiEditResponse *openai_image_edit.Response
	err = utils.DecodeJSON(res.Body, &openAiEditResponse)
	if err != nil {
		return nil, err
	}

	if openAiEditResponse.Error != nil {
		return nil, errors.New(openAiEditResponse.Error.Message)
	}

	return openAiEditResponse.ToNativeResponse(), nil
}
