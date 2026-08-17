package gemini_image_edit

import (
	"encoding/base64"
	"mime"
	"path/filepath"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/image_edit"
	gemini_responses2 "github.com/hastekit/agent-sdk-go/pkg/gateway/providers/gemini/gemini_responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

// Request represents a Gemini Nano Banana image edit request.
// Uses the same generateContent endpoint with image + text prompt.
// Endpoint: POST /v1beta/models/{model}:generateContent
type Request struct {
	Contents         []gemini_responses2.Content `json:"contents"`
	GenerationConfig GenerationConfig            `json:"generationConfig"`
}

type GenerationConfig struct {
	ResponseModalities []string     `json:"responseModalities"`
	ImageConfig        *ImageConfig `json:"imageConfig,omitempty"`
}

type ImageConfig struct {
	AspectRatio *string `json:"aspectRatio,omitempty"`
	ImageSize   *string `json:"imageSize,omitempty"`
}

func NativeRequestToRequest(in *image_edit.Request) *Request {
	// Build parts: text instruction + inline images
	parts := []gemini_responses2.Part{
		{
			Text: &in.Prompt,
		},
	}

	// Add each image as an inline data part
	for _, img := range in.Images {
		mimeType := "image/png"
		if img.Filename != "" {
			if detected := mime.TypeByExtension(filepath.Ext(img.Filename)); detected != "" {
				mimeType = detected
			}
		}
		imageBase64 := base64.StdEncoding.EncodeToString(img.Data)
		parts = append(parts, gemini_responses2.Part{
			InlineData: &gemini_responses2.InlinePartData{
				MimeType: mimeType,
				Data:     imageBase64,
			},
		})
	}

	req := &Request{
		Contents: []gemini_responses2.Content{
			{
				Parts: parts,
			},
		},
		GenerationConfig: GenerationConfig{
			ResponseModalities: []string{"TEXT", "IMAGE"},
		},
	}

	// Map aspect ratio and resolution to imageConfig
	if in.AspectRatio != nil || in.Resolution != nil {
		req.GenerationConfig.ImageConfig = &ImageConfig{
			AspectRatio: in.AspectRatio,
			ImageSize:   in.Resolution,
		}
	}

	return req
}

// Response represents a Gemini Nano Banana image edit response.
type Response struct {
	*gemini_responses2.Response
}

func (r *Response) ToNativeResponse() *image_edit.Response {
	var images []image_edit.Image

	if len(r.Candidates) > 0 {
		for _, part := range r.Candidates[0].Content.Parts {
			if part.InlineData != nil {
				images = append(images, image_edit.Image{
					B64JSON:  utils.Ptr(part.InlineData.Data),
					MimeType: utils.Ptr(part.InlineData.MimeType),
				})
			}
		}
	}

	resp := &image_edit.Response{
		Images: images,
		RawFields: map[string]any{
			"Gemini": r,
		},
	}

	if r.UsageMetadata != nil {
		resp.Usage = &image_edit.Usage{
			InputTokens:  r.UsageMetadata.PromptTokenCount,
			OutputTokens: r.UsageMetadata.CandidatesTokenCount,
			TotalTokens:  r.UsageMetadata.TotalTokenCount,
		}
	}

	return resp
}
