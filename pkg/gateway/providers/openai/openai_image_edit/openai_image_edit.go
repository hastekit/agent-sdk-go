package openai_image_edit

import (
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/image_edit"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

// Response represents an OpenAI image edit response.
// Endpoint: POST /v1/images/edits (multipart/form-data)
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

func (r *Response) ToNativeResponse() *image_edit.Response {
	images := make([]image_edit.Image, len(r.Data))
	for i, d := range r.Data {
		img := image_edit.Image{
			B64JSON:       d.B64JSON,
			URL:           d.URL,
			RevisedPrompt: d.RevisedPrompt,
		}
		if d.B64JSON != nil {
			img.MimeType = utils.Ptr("image/png")
		}
		images[i] = img
	}

	resp := &image_edit.Response{
		Images: images,
	}

	if r.Usage != nil {
		resp.Usage = &image_edit.Usage{
			InputTokens:  r.Usage.InputTokens,
			OutputTokens: r.Usage.OutputTokens,
			TotalTokens:  r.Usage.TotalTokens,
		}
	}

	return resp
}
