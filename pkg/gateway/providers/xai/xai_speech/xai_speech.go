package xai_speech

import (
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/speech"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

type Request struct {
	Text         string        `json:"text"`
	VoiceID      string        `json:"voice_id"`
	Language     *string       `json:"language,omitempty"`
	OutputFormat *OutputFormat `json:"output_format"`
}

type OutputFormat struct {
	Codec      string `json:"codec"`
	SampleRate *int   `json:"sample_rate,omitempty"`
	BitRate    *int   `json:"bit_rate,omitempty"`
}

func (r *Request) ToNativeRequest() *speech.Request {
	respFormat := "mp3"
	if r.OutputFormat != nil {
		respFormat = r.OutputFormat.Codec
	}

	return &speech.Request{
		Input:          r.Text,
		Model:          "",
		Voice:          r.VoiceID,
		Language:       r.Language,
		Instruction:    nil,
		ResponseFormat: utils.Ptr(respFormat),
		Speed:          nil,
		StreamFormat:   nil,
	}
}

func NativeRequestToRequest(in *speech.Request) *Request {
	outputFormat := &OutputFormat{
		Codec: "mp3",
	}

	if in.ResponseFormat != nil {
		outputFormat = &OutputFormat{
			Codec: *in.ResponseFormat,
		}
	}

	return &Request{
		Text:         in.Input,
		VoiceID:      in.Voice,
		Language:     in.Language,
		OutputFormat: outputFormat,
	}
}

type Response struct {
	AudioData []byte `json:"audio_data"`
}

func (r *Response) ToNativeResponse() *speech.Response {
	return &speech.Response{
		Audio:       r.AudioData,
		ContentType: "",
		Usage:       speech.Usage{},
		RawFields:   nil,
	}
}

func NativeResponseToResponse(in *speech.Response) *Response {
	return &Response{
		AudioData: in.Audio,
	}
}

type ResponseChunk struct {
	speech.ResponseChunk
}

func (r *ResponseChunk) ToNativeResponse() *speech.ResponseChunk {
	return &r.ResponseChunk
}
