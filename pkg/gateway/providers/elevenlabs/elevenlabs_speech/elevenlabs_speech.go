package elevenlabs_speech

import (
	"net/http"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/speech"
)

type Request struct {
	Text          string         `json:"text"`
	ModelID       string         `json:"model_id,omitempty"`
	LanguageCode  *string        `json:"language_code,omitempty"`
	VoiceSettings *VoiceSettings `json:"voice_settings,omitempty"`
}

type VoiceSettings struct {
	Stability       *float64 `json:"stability,omitempty"`
	SimilarityBoost *float64 `json:"similarity_boost,omitempty"`
	Style           *float64 `json:"style,omitempty"`
	Speed           *float64 `json:"speed,omitempty"`
	UseSpeakerBoost *bool    `json:"use_speaker_boost,omitempty"`
}

func NativeRequestToRequest(in *speech.Request) *Request {
	var voiceSettings *VoiceSettings
	if in.Speed != nil {
		speed := float64(*in.Speed)
		voiceSettings = &VoiceSettings{
			Speed: &speed,
		}
	}

	modelID := in.Model
	if modelID == "" {
		modelID = "eleven_multilingual_v2"
	}

	return &Request{
		Text:          in.Input,
		ModelID:       modelID,
		LanguageCode:  in.Language,
		VoiceSettings: voiceSettings,
	}
}

type Response struct {
	AudioData []byte `json:"audio_data"`
}

func (r *Response) ToNativeResponse() *speech.Response {
	return &speech.Response{
		Audio:       r.AudioData,
		ContentType: http.DetectContentType(r.AudioData),
		Usage:       speech.Usage{},
		RawFields:   nil,
	}
}

func NativeResponseFormatToResponseFormat(in *string) string {
	if in == nil {
		return "mp3_44100_128"
	}

	switch *in {
	case "pcm":
		return "pcm_16000"
	}

	return "mp3_44100_128"
}
