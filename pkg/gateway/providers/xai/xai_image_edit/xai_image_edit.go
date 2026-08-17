package xai_image_edit

import (
	"encoding/base64"
	"fmt"
	"mime"
	"path/filepath"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/image_edit"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

// Request represents an xAI image edit request.
// Endpoint: POST /v1/images/edits (application/json, NOT multipart)
type Request struct {
	Prompt         string     `json:"prompt"`
	Model          string     `json:"model"`
	Image          ImageInput `json:"image"`
	N              *int       `json:"n,omitempty"`
	AspectRatio    *string    `json:"aspect_ratio,omitempty"`    // "1:1", "16:9", "9:16", "4:3", "3:2", "auto"
	Resolution     *string    `json:"resolution,omitempty"`      // "1k", "2k"
	ResponseFormat *string    `json:"response_format,omitempty"` // "url" or "b64_json"
}

type ImageInput struct {
	URL  string `json:"url"`  // Can be a URL or a base64 data URI
	Type string `json:"type"` // "image_url"
}

func NativeRequestToRequest(in *image_edit.Request) *Request {
	model := in.Model
	if model == "" {
		model = "grok-2-image"
	}

	// xAI only supports a single image; use the first one
	var imageInput ImageInput
	if len(in.Images) > 0 {
		img := in.Images[0]
		mimeType := "image/png"
		if img.Filename != "" {
			if detected := mime.TypeByExtension(filepath.Ext(img.Filename)); detected != "" {
				mimeType = detected
			}
		}

		// Encode image as base64 data URI for xAI
		imageBase64 := base64.StdEncoding.EncodeToString(img.Data)
		dataURI := fmt.Sprintf("data:%s;base64,%s", mimeType, imageBase64)
		imageInput = ImageInput{
			URL:  dataURI,
			Type: "image_url",
		}
	}

	return &Request{
		Prompt:         in.Prompt,
		Model:          model,
		Image:          imageInput,
		N:              in.N,
		AspectRatio:    in.AspectRatio,
		Resolution:     in.Resolution,
		ResponseFormat: in.ResponseFormat,
	}
}

// Response represents an xAI image edit response.
type Response struct {
	Data []ResponseData `json:"data"`
}

type ResponseData struct {
	B64JSON       *string `json:"b64_json,omitempty"`
	URL           *string `json:"url,omitempty"`
	RevisedPrompt *string `json:"revised_prompt,omitempty"`
}

func (r *Response) ToNativeResponse() *image_edit.Response {
	images := make([]image_edit.Image, len(r.Data))
	for i, d := range r.Data {
		images[i] = image_edit.Image{
			B64JSON:       d.B64JSON,
			URL:           d.URL,
			RevisedPrompt: d.RevisedPrompt,
			MimeType:      utils.Ptr("image/png"),
		}
	}

	return &image_edit.Response{
		Images: images,
	}
}
