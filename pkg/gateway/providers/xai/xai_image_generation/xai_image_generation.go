package xai_image_generation

import (
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/image_generation"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

// Request represents an xAI image generation request.
// Endpoint: POST /v1/images/generations
type Request struct {
	Prompt         string  `json:"prompt"`
	Model          string  `json:"model"`
	N              *int    `json:"n,omitempty"`
	AspectRatio    *string `json:"aspect_ratio,omitempty"`    // "1:1", "16:9", "9:16", "4:3", "3:2", "auto"
	Resolution     *string `json:"resolution,omitempty"`      // "1k", "2k"
	ResponseFormat *string `json:"response_format,omitempty"` // "url" or "b64_json"
}

func NativeRequestToRequest(in *image_generation.Request) *Request {
	model := in.Model
	if model == "" {
		model = "grok-2-image"
	}

	return &Request{
		Prompt:         in.Prompt,
		Model:          model,
		N:              in.N,
		AspectRatio:    in.AspectRatio,
		Resolution:     in.Resolution,
		ResponseFormat: in.ResponseFormat,
	}
}

// Response represents an xAI image generation response.
type Response struct {
	Created int            `json:"created"`
	Data    []ResponseData `json:"data"`
}

type ResponseData struct {
	B64JSON       *string `json:"b64_json,omitempty"`
	URL           *string `json:"url,omitempty"`
	RevisedPrompt *string `json:"revised_prompt,omitempty"`
}

func (r *Response) ToNativeResponse() *image_generation.Response {
	images := make([]image_generation.Image, len(r.Data))
	for i, d := range r.Data {
		images[i] = image_generation.Image{
			B64JSON:       d.B64JSON,
			URL:           d.URL,
			RevisedPrompt: d.RevisedPrompt,
			MimeType:      utils.Ptr("image/png"),
		}
	}

	return &image_generation.Response{
		Images: images,
	}
}
