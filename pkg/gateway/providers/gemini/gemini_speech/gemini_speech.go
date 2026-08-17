package gemini_speech

import (
	"encoding/base64"

	speech2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/speech"
	gemini_responses2 "github.com/hastekit/agent-sdk-go/pkg/gateway/providers/gemini/gemini_responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

type Request struct {
	Model            string                      `json:"model"`
	Contents         []gemini_responses2.Content `json:"contents"`
	GenerationConfig GenerationConfig            `json:"generationConfig"`
}

type GenerationConfig struct {
	ResponseModalities []string          `json:"responseModalities"` // "AUDIO"
	SpeechConfig       SpeechConfigParam `json:"speechConfig"`
}

type SpeechConfigParam struct {
	VoiceConfig  VoiceConfigParam `json:"voiceConfig"`
	LanguageCode *string          `json:"languageCode,omitempty"`
}

type VoiceConfigParam struct {
	PrebuiltVoiceConfig PrebuiltVoiceConfig `json:"prebuiltVoiceConfig"`
}

type PrebuiltVoiceConfig struct {
	VoiceName string `json:"voiceName"`
}

func (r *Request) ToNativeRequest() *speech2.Request {
	return &speech2.Request{
		Input:    r.Contents[0].String(),
		Model:    r.Model,
		Voice:    r.GenerationConfig.SpeechConfig.VoiceConfig.PrebuiltVoiceConfig.VoiceName,
		Language: r.GenerationConfig.SpeechConfig.LanguageCode,
	}
}

func NativeRequestToRequest(in *speech2.Request) *Request {
	return &Request{
		Model: in.Model,
		Contents: []gemini_responses2.Content{
			{
				Parts: []gemini_responses2.Part{
					{
						Text: utils.Ptr("Text-To-Speech the following text:"),
					},
					{
						Text: &in.Input,
					},
				},
			},
		},
		GenerationConfig: GenerationConfig{
			ResponseModalities: []string{"AUDIO"},
			SpeechConfig: SpeechConfigParam{
				VoiceConfig: VoiceConfigParam{
					PrebuiltVoiceConfig: PrebuiltVoiceConfig{
						VoiceName: in.Voice,
					},
				},
				LanguageCode: in.Language,
			},
		},
	}
}

type Response struct {
	*gemini_responses2.Response
}

func (r *Response) ToNativeResponse(responseFormat *string) *speech2.Response {
	audioData := r.Candidates[0].Content.Parts[0].InlineData.Data

	audioBinaryData, err := base64.StdEncoding.DecodeString(audioData)
	if err != nil {
		return nil
	}

	contentType := "audio/pcm; rate=24000; channels=1"
	if responseFormat != nil && *responseFormat == "wav" {
		contentType = "audio/wav"
		audioBinaryData, err = utils.Base64PCMToWAV(audioData, 24000, 1, 16)
		if err != nil {
			return nil
		}
	}

	return &speech2.Response{
		Audio:       audioBinaryData,
		ContentType: contentType,
		Usage: speech2.Usage{
			InputTokens: r.UsageMetadata.PromptTokenCount,
			InputTokensDetails: struct {
				CachedTokens int `json:"cached_tokens"`
			}{CachedTokens: 0},
			OutputTokens: r.UsageMetadata.CandidatesTokenCount,
			TotalTokens:  r.UsageMetadata.TotalTokenCount,
			OutputTokensDetails: struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			}{ReasoningTokens: r.UsageMetadata.ThoughtsTokenCount},
		},
		RawFields: map[string]interface{}{
			"Gemini": r,
		},
	}
}

func (r *Response) ToNativeAudio(responseFormat *string) ([]byte, string, error) {
	audioData := r.Candidates[0].Content.Parts[0].InlineData.Data

	pcmData, err := base64.StdEncoding.DecodeString(audioData)
	if err != nil {
		return nil, "", err
	}

	if responseFormat == nil {
		return pcmData, "audio/L16; codec=pcm; rate=24000; channels=1", nil
	}

	// convert pcm to wav
	switch *responseFormat {
	case "wav":
		wavData, err := utils.Base64PCMToWAV(audioData, 24000, 1, 16)
		if err != nil {
			return nil, "", err
		}
		return wavData, "audio/wav", nil

	default:
		return pcmData, "audio/pcm; rate=24000; channels=1", nil
	}
}

func NativeResponseToResponse(in *speech2.Response) *Response {
	if raw, exists := in.RawFields["Gemini"]; exists {
		if geminiResp, ok := raw.(*Response); ok {
			return geminiResp
		}
	}

	return &Response{
		Response: &gemini_responses2.Response{
			Candidates: []gemini_responses2.Candidate{
				{
					Content: gemini_responses2.Content{
						Parts: []gemini_responses2.Part{
							{
								InlineData: &gemini_responses2.InlinePartData{
									Data:     base64.StdEncoding.EncodeToString(in.Audio),
									MimeType: in.ContentType,
								},
							},
						},
					},
				},
			},
			UsageMetadata: &gemini_responses2.UsageMetadata{
				PromptTokenCount:     in.Usage.InputTokens,
				CandidatesTokenCount: in.Usage.OutputTokens,
				TotalTokenCount:      in.Usage.TotalTokens,
				PromptTokensDetails:  nil,
				ThoughtsTokenCount:   in.Usage.OutputTokensDetails.ReasoningTokens,
			},
			ModelVersion: "",
			ResponseID:   "",
		},
	}
}

type NativeResponseChunkToResponseChunkConverter struct {
	audioData string
}

func (n *NativeResponseChunkToResponseChunkConverter) NativeResponseChunkToResponseChunk(in *speech2.ResponseChunk) []Response {
	if in.OfAudioDelta != nil {
		n.audioData += in.OfAudioDelta.Audio
		return []Response{}
	}

	return []Response{
		{
			Response: &gemini_responses2.Response{
				Candidates: []gemini_responses2.Candidate{
					{
						Content: gemini_responses2.Content{
							Parts: []gemini_responses2.Part{
								{
									InlineData: &gemini_responses2.InlinePartData{
										Data:     n.audioData,
										MimeType: "audio/pcm; rate=24000; channels=1",
									},
								},
							},
						},
					},
				},
				UsageMetadata: &gemini_responses2.UsageMetadata{
					PromptTokenCount:     in.OfAudioDone.Usage.InputTokens,
					CandidatesTokenCount: in.OfAudioDone.Usage.OutputTokens,
					TotalTokenCount:      in.OfAudioDone.Usage.TotalTokens,
					PromptTokensDetails:  nil,
					ThoughtsTokenCount:   in.OfAudioDone.Usage.OutputTokensDetails.ReasoningTokens,
				},
				ModelVersion: "",
				ResponseID:   "",
			},
		},
	}
}

type ResponseChunkToNativeResponseChunkConverter struct {
	usageMetadata *gemini_responses2.UsageMetadata
}

func (c *ResponseChunkToNativeResponseChunkConverter) ResponseChunkToNativeResponseChunk(in *Response) []*speech2.ResponseChunk {
	if in == nil {
		return []*speech2.ResponseChunk{
			{
				OfAudioDone: &speech2.ChunkAudioDone[speech2.ChunkTypeAudioDone]{
					Usage: speech2.Usage{
						InputTokens: c.usageMetadata.PromptTokenCount,
						InputTokensDetails: struct {
							CachedTokens int `json:"cached_tokens"`
						}{CachedTokens: 0},
						OutputTokens: c.usageMetadata.CandidatesTokenCount,
						TotalTokens:  c.usageMetadata.TotalTokenCount,
						OutputTokensDetails: struct {
							ReasoningTokens int `json:"reasoning_tokens"`
						}{ReasoningTokens: c.usageMetadata.ThoughtsTokenCount},
					},
				},
			},
		}
	}

	c.usageMetadata = in.UsageMetadata

	return []*speech2.ResponseChunk{
		{
			OfAudioDelta: &speech2.ChunkAudioDelta[speech2.ChunkTypeAudioDelta]{
				Audio: in.Candidates[0].Content.Parts[0].InlineData.Data,
			},
		},
	}
}
