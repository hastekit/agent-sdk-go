package xai

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/bytedance/sonic"
	image_edit2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/image_edit"
	image_generation2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/image_generation"
	responses2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	speech2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/speech"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/base"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/xai/xai_image_edit"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/xai/xai_image_generation"
	xai_responses2 "github.com/hastekit/agent-sdk-go/pkg/gateway/providers/xai/xai_responses"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/xai/xai_speech"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

type ClientOptions struct {
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
		opts.BaseURL = "https://api.x.ai/v1"
	}

	return &Client{
		opts: opts,
	}
}

func (c *Client) NewResponses(ctx context.Context, inp *responses2.Request) (*responses2.Response, error) {
	xaiRequest := xai_responses2.NativeRequestToRequest(inp)

	payload, err := sonic.Marshal(xaiRequest)
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
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		var errResp map[string]any
		err = utils.DecodeJSON(res.Body, &errResp)
		return nil, fmt.Errorf("error: %v", errResp)
	}

	var xaiResponse *xai_responses2.Response
	err = utils.DecodeJSON(res.Body, &xaiResponse)
	if err != nil {
		return nil, err
	}

	if xaiResponse.Error != nil {
		return nil, errors.New(xaiResponse.Error.Message)
	}

	return xaiResponse.ToNativeResponse(), nil
}

func (c *Client) NewStreamingResponses(ctx context.Context, inp *responses2.Request) (chan *responses2.ResponseChunk, error) {
	xaiRequest := xai_responses2.NativeRequestToRequest(inp)

	payload, err := sonic.Marshal(xaiRequest)
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
		var errResp string
		err = utils.DecodeJSON(res.Body, &errResp)
		return nil, errors.New(errResp)
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
			if strings.HasPrefix(line, "data:") {
				xaiResponseChunk := &xai_responses2.ResponseChunk{}
				err = sonic.Unmarshal([]byte(strings.TrimPrefix(line, "data:")), xaiResponseChunk)
				if err != nil {
					slog.WarnContext(ctx, "unable to unmarshal xai response chunk", slog.String("data", line), slog.Any("error", err))
					continue
				}
				out <- xaiResponseChunk.ToNativeResponseChunk()
			}
		}
	}()

	return out, nil
}

func (c *Client) NewSpeech(ctx context.Context, in *speech2.Request) (*speech2.Response, error) {
	xaiRequest := xai_speech.NativeRequestToRequest(in)

	payload, err := sonic.Marshal(xaiRequest)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, c.opts.BaseURL+"/tts", bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+c.opts.ApiKey)
	req.Header.Set("Content-Type", "application/json")

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

	var reader io.Reader = res.Body
	if res.Header.Get("Content-Encoding") == "gzip" {
		gzipReader, err := gzip.NewReader(res.Body)
		if err != nil {
			return nil, err
		}
		defer gzipReader.Close()
		reader = gzipReader
	}

	audioData, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	xaiResponse := &xai_speech.Response{
		AudioData: audioData,
	}

	return xaiResponse.ToNativeResponse(), nil
}

func (c *Client) NewImageGeneration(ctx context.Context, in *image_generation2.Request) (*image_generation2.Response, error) {
	xaiRequest := xai_image_generation.NativeRequestToRequest(in)

	payload, err := sonic.Marshal(xaiRequest)
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

	var xaiResponse *xai_image_generation.Response
	err = utils.DecodeJSON(res.Body, &xaiResponse)
	if err != nil {
		return nil, err
	}

	return xaiResponse.ToNativeResponse(), nil
}

func (c *Client) NewImageEdit(ctx context.Context, in *image_edit2.Request) (*image_edit2.Response, error) {
	xaiRequest := xai_image_edit.NativeRequestToRequest(in)

	payload, err := sonic.Marshal(xaiRequest)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, c.opts.BaseURL+"/images/edits", bytes.NewBuffer(payload))
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

	var xaiEditResponse *xai_image_edit.Response
	err = utils.DecodeJSON(res.Body, &xaiEditResponse)
	if err != nil {
		return nil, err
	}

	return xaiEditResponse.ToNativeResponse(), nil
}
