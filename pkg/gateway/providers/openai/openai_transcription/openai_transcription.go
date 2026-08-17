package openai_transcription

import (
	transcription2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/transcription"
)

type Request struct {
	transcription2.Request
}

func (r *Request) ToNativeRequest() *transcription2.Request {
	return &r.Request
}

func NativeRequestToRequest(in *transcription2.Request) *Request {
	return &Request{*in}
}

// Response represents the OpenAI transcription API response (verbose_json format)
type Response struct {
	Task     string    `json:"task"`
	Language string    `json:"language"`
	Duration float64   `json:"duration"`
	Text     string    `json:"text"`
	Words    []Word    `json:"words,omitempty"`
	Segments []Segment `json:"segments,omitempty"`
}

type Word struct {
	Word  string  `json:"word"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

type Segment struct {
	ID               int     `json:"id"`
	Seek             int     `json:"seek"`
	Start            float64 `json:"start"`
	End              float64 `json:"end"`
	Text             string  `json:"text"`
	Temperature      float64 `json:"temperature"`
	AvgLogprob       float64 `json:"avg_logprob"`
	CompressionRatio float64 `json:"compression_ratio"`
	NoSpeechProb     float64 `json:"no_speech_prob"`
}

func (r *Response) ToNativeResponse() *transcription2.Response {
	lang := r.Language
	dur := r.Duration

	resp := &transcription2.Response{
		Text:     r.Text,
		Language: &lang,
		Duration: &dur,
	}

	for _, w := range r.Words {
		resp.Words = append(resp.Words, transcription2.Word{
			Word:  w.Word,
			Start: w.Start,
			End:   w.End,
		})
	}

	for _, s := range r.Segments {
		resp.Segments = append(resp.Segments, transcription2.Segment{
			ID:               s.ID,
			Seek:             s.Seek,
			Start:            s.Start,
			End:              s.End,
			Text:             s.Text,
			Temperature:      s.Temperature,
			AvgLogprob:       s.AvgLogprob,
			CompressionRatio: s.CompressionRatio,
			NoSpeechProb:     s.NoSpeechProb,
		})
	}

	return resp
}
