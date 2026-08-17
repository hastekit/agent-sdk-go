package sarvam_transcription

import (
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/transcription"
)

// LanguageCodeUnknown asks Sarvam to auto-detect the spoken language.
const LanguageCodeUnknown = "unknown"

// Response is the body of a successful POST /speech-to-text.
type Response struct {
	RequestID           string      `json:"request_id"`
	Transcript          string      `json:"transcript"`
	LanguageCode        string      `json:"language_code"`
	LanguageProbability float64     `json:"language_probability"`
	Timestamps          *Timestamps `json:"timestamps"`
}

// Timestamps is Sarvam's column-oriented timing: three parallel arrays
// rather than one array of objects.
type Timestamps struct {
	Words            []string  `json:"words"`
	StartTimeSeconds []float64 `json:"start_time_seconds"`
	EndTimeSeconds   []float64 `json:"end_time_seconds"`
}

func (r *Response) ToNativeResponse() *transcription.Response {
	out := &transcription.Response{
		Text: r.Transcript,
		Raw:  map[string]any{"request_id": r.RequestID},
	}

	if r.LanguageCode != "" && r.LanguageCode != LanguageCodeUnknown {
		lang := r.LanguageCode
		out.Language = &lang
	}

	if r.Timestamps == nil {
		return out
	}

	for i, word := range r.Timestamps.Words {
		entry := transcription.Word{Word: word}
		if i < len(r.Timestamps.StartTimeSeconds) {
			entry.Start = r.Timestamps.StartTimeSeconds[i]
		}
		if i < len(r.Timestamps.EndTimeSeconds) {
			entry.End = r.Timestamps.EndTimeSeconds[i]
		}

		out.Words = append(out.Words, entry)
	}

	return out
}
