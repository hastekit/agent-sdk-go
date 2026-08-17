package openai_image_generation

import (
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/image_generation"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

type Request struct {
	Prompt         string  `json:"prompt"`
	Model          string  `json:"model"`
	N              *int    `json:"n,omitempty"`
	Size           *string `json:"size,omitempty"`
	Quality        *string `json:"quality,omitempty"`
	Style          *string `json:"style,omitempty"`
	ResponseFormat *string `json:"response_format,omitempty"`
	Background     *string `json:"background,omitempty"`
	OutputFormat   *string `json:"output_format,omitempty"`
}

func NativeRequestToRequest(in *image_generation.Request) *Request {
	req := &Request{
		Prompt:       in.Prompt,
		Model:        in.Model,
		N:            in.N,
		Size:         in.Size,
		Quality:      in.Quality,
		Style:        in.Style,
		Background:   in.Background,
		OutputFormat: in.OutputFormat,
	}

	// Only dall-e-2 and dall-e-3 support response_format
	// Other models do not support response_format, it always returns b64_json
	if in.Model == "dall-e-2" || in.Model == "dall-e-3" {
		req.ResponseFormat = in.ResponseFormat
	}

	return req
}

type Response struct {
	Created int            `json:"created"`
	Data    []ResponseData `json:"data"`
	Usage   *ResponseUsage `json:"usage,omitempty"`
	Error   *ResponseError `json:"error,omitempty"`
}

type ResponseData struct {
	B64JSON       *string `json:"b64_json,omitempty"`
	URL           *string `json:"url,omitempty"`
	RevisedPrompt *string `json:"revised_prompt,omitempty"`
}

type ResponseUsage struct {
	TotalTokens        int `json:"total_tokens"`
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	InputTokensDetails struct {
		TextTokens  int `json:"text_tokens"`
		ImageTokens int `json:"image_tokens"`
	} `json:"input_tokens_details"`
}

type ResponseError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

func (r *Response) ToNativeResponse() *image_generation.Response {
	images := make([]image_generation.Image, len(r.Data))
	for i, d := range r.Data {
		img := image_generation.Image{
			B64JSON:       d.B64JSON,
			URL:           d.URL,
			RevisedPrompt: d.RevisedPrompt,
		}
		if d.B64JSON != nil {
			img.MimeType = utils.Ptr("image/png")
		}
		images[i] = img
	}

	resp := &image_generation.Response{
		Images: images,
	}

	if r.Usage != nil {
		resp.Usage = &image_generation.Usage{
			InputTokens:  r.Usage.InputTokens,
			OutputTokens: r.Usage.OutputTokens,
			TotalTokens:  r.Usage.TotalTokens,
		}
	}

	return resp
}
