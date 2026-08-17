package gemini_image_generation

import (
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/image_generation"
	gemini_responses2 "github.com/hastekit/agent-sdk-go/pkg/gateway/providers/gemini/gemini_responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

// Request represents a Gemini Nano Banana image generation request.
// Uses the generateContent endpoint with responseModalities: ["TEXT", "IMAGE"]
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
	AspectRatio *string `json:"aspectRatio,omitempty"` // "1:1", "16:9", "9:16", "4:3", "3:4", etc.
	ImageSize   *string `json:"imageSize,omitempty"`   // "512", "1K", "2K", "4K"
}

func NativeRequestToRequest(in *image_generation.Request) *Request {
	req := &Request{
		Contents: []gemini_responses2.Content{
			{
				Parts: []gemini_responses2.Part{
					{
						Text: &in.Prompt,
					},
				},
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

// Response represents a Gemini Nano Banana image generation response.
// Images are returned as base64 inline data in the candidates parts.
type Response struct {
	*gemini_responses2.Response
}

func (r *Response) ToNativeResponse() *image_generation.Response {
	var images []image_generation.Image

	if len(r.Candidates) > 0 {
		for _, part := range r.Candidates[0].Content.Parts {
			if part.InlineData != nil {
				images = append(images, image_generation.Image{
					B64JSON:  utils.Ptr(part.InlineData.Data),
					MimeType: utils.Ptr(part.InlineData.MimeType),
				})
			}
		}
	}

	resp := &image_generation.Response{
		Images: images,
		RawFields: map[string]any{
			"Gemini": r,
		},
	}

	if r.UsageMetadata != nil {
		resp.Usage = &image_generation.Usage{
			InputTokens:  r.UsageMetadata.PromptTokenCount,
			OutputTokens: r.UsageMetadata.CandidatesTokenCount,
			TotalTokens:  r.UsageMetadata.TotalTokenCount,
		}
	}

	return resp
}
