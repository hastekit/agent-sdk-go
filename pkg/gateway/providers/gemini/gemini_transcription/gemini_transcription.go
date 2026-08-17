package gemini_transcription

import (
	"encoding/base64"
	"mime"
	"path/filepath"

	transcription2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/transcription"
	gemini_responses2 "github.com/hastekit/agent-sdk-go/pkg/gateway/providers/gemini/gemini_responses"
)

type Request struct {
	Contents         []gemini_responses2.Content         `json:"contents"`
	GenerationConfig *gemini_responses2.GenerationConfig `json:"generationConfig,omitempty"`
}

func NativeRequestToRequest(in *transcription2.Request) *Request {
	mimeType := mimeTypeFromFilename(in.AudioFilename)
	audioBase64 := base64.StdEncoding.EncodeToString(in.Audio)

	prompt := "Transcribe this audio. Return only the transcribed text, nothing else."
	if in.Language != nil {
		prompt = "Transcribe this audio in " + *in.Language + ". Return only the transcribed text, nothing else."
	}
	if in.Prompt != nil {
		prompt = *in.Prompt
	}

	return &Request{
		Contents: []gemini_responses2.Content{
			{
				Parts: []gemini_responses2.Part{
					{
						InlineData: &gemini_responses2.InlinePartData{
							MimeType: mimeType,
							Data:     audioBase64,
						},
					},
					{
						Text: &prompt,
					},
				},
			},
		},
	}
}

type Response struct {
	*gemini_responses2.Response
}

func (r *Response) ToNativeResponse() *transcription2.Response {
	if len(r.Candidates) == 0 || len(r.Candidates[0].Content.Parts) == 0 {
		return &transcription2.Response{}
	}

	text := ""
	for _, part := range r.Candidates[0].Content.Parts {
		if part.Text != nil {
			text += *part.Text
		}
	}

	resp := &transcription2.Response{
		Text: text,
		Raw:  map[string]any{"Gemini": r},
	}

	if r.UsageMetadata != nil {
		resp.Usage = &transcription2.Usage{
			PromptTokens:     r.UsageMetadata.PromptTokenCount,
			CompletionTokens: r.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      r.UsageMetadata.TotalTokenCount,
		}
	}

	return resp
}

func mimeTypeFromFilename(filename string) string {
	if filename == "" {
		return "audio/mpeg"
	}

	ext := filepath.Ext(filename)
	if ext == "" {
		return "audio/mpeg"
	}

	mimeType := mime.TypeByExtension(ext)
	if mimeType == "" {
		switch ext {
		case ".mp3":
			return "audio/mpeg"
		case ".wav":
			return "audio/wav"
		case ".flac":
			return "audio/flac"
		case ".ogg":
			return "audio/ogg"
		case ".m4a":
			return "audio/mp4"
		case ".aac":
			return "audio/aac"
		case ".webm":
			return "audio/webm"
		case ".pcm":
			return "audio/L16"
		default:
			return "audio/mpeg"
		}
	}

	return mimeType
}
