package sarvam_speech

import (
	"encoding/base64"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/speech"
)

const (
	// DefaultModel is Sarvam's broadly available TTS model. Callers wanting
	// bulbul:v3 (longer input, temperature control) pass it as the model.
	DefaultModel = "bulbul:v2"
	// DefaultLanguageCode is used when the request carries no language:
	// language_code is required by the API and has no server-side default.
	DefaultLanguageCode = "en-IN"
)

// Request is the body of POST /text-to-speech.
type Request struct {
	Text             string   `json:"text"`
	LanguageCode     string   `json:"language_code"`
	Model            string   `json:"model,omitempty"`
	Speaker          string   `json:"speaker,omitempty"`
	Pace             *float64 `json:"pace,omitempty"`
	SpeechSampleRate *int     `json:"speech_sample_rate,omitempty"`
	OutputAudioCodec string   `json:"output_audio_codec,omitempty"`
}

func NativeRequestToRequest(in *speech.Request) *Request {
	out := &Request{
		Text:             in.Input,
		LanguageCode:     DefaultLanguageCode,
		Model:            in.Model,
		Speaker:          in.Voice,
		OutputAudioCodec: NativeResponseFormatToCodec(in.ResponseFormat),
	}

	if out.Model == "" {
		out.Model = DefaultModel
	}

	if in.Language != nil && *in.Language != "" {
		out.LanguageCode = *in.Language
	}

	if in.Speed != nil {
		pace := float64(*in.Speed)
		out.Pace = &pace
	}

	return out
}

// Response is the body of a successful POST /text-to-speech. Sarvam returns
// the audio base64-encoded inside JSON rather than as a binary body.
type Response struct {
	RequestID string   `json:"request_id"`
	Audios    []string `json:"audios"`
}

func (r *Response) ToNativeResponse(codec string) *speech.Response {
	var audio []byte
	for _, encoded := range r.Audios {
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			continue
		}
		audio = append(audio, decoded...)
	}

	return &speech.Response{
		Audio:       audio,
		ContentType: CodecToContentType(codec),
		RawFields:   map[string]any{"request_id": r.RequestID},
	}
}

// NativeResponseFormatToCodec maps the native response format onto Sarvam's
// output_audio_codec enum.
func NativeResponseFormatToCodec(in *string) string {
	if in == nil {
		return "wav"
	}

	switch *in {
	case "mp3", "opus", "flac", "aac", "wav", "mulaw", "alaw":
		return *in
	case "pcm":
		return "linear16"
	default:
		return "wav"
	}
}

func CodecToContentType(codec string) string {
	switch codec {
	case "mp3":
		return "audio/mpeg"
	case "opus":
		return "audio/opus"
	case "flac":
		return "audio/flac"
	case "aac":
		return "audio/aac"
	case "linear16", "mulaw", "alaw":
		return "audio/L16"
	default:
		return "audio/wav"
	}
}
