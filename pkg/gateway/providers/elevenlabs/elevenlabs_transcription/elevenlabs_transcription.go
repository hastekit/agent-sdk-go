package elevenlabs_transcription

import (
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/transcription"
)

// Response represents the ElevenLabs speech-to-text API response
type Response struct {
	LanguageCode        string  `json:"language_code"`
	LanguageProbability float64 `json:"language_probability"`
	Text                string  `json:"text"`
	Words               []Word  `json:"words,omitempty"`
}

type Word struct {
	Text      string  `json:"text"`
	Start     float64 `json:"start"`
	End       float64 `json:"end"`
	Type      string  `json:"type"`
	SpeakerID string  `json:"speaker_id,omitempty"`
}

func (r *Response) ToNativeResponse() *transcription.Response {
	lang := r.LanguageCode

	resp := &transcription.Response{
		Text:     r.Text,
		Language: &lang,
	}

	for _, w := range r.Words {
		if w.Type == "word" {
			resp.Words = append(resp.Words, transcription.Word{
				Word:  w.Text,
				Start: w.Start,
				End:   w.End,
			})
		}
	}

	return resp
}
