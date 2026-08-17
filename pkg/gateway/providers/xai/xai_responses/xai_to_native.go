package xai_responses

import (
	responses2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

func (in *Request) ToNativeRequest() *responses2.Request {
	return &responses2.Request{
		Model:        in.Model,
		Input:        in.Input,
		Instructions: in.Instructions,
		Tools:        in.Tools,
		Parameters: responses2.Parameters{
			Background:        in.Background,
			MaxOutputTokens:   in.MaxOutputTokens,
			MaxToolCalls:      in.MaxToolCalls,
			ParallelToolCalls: in.ParallelToolCalls,
			Store:             in.Store,
			Temperature:       in.Temperature,
			TopLogprobs:       in.TopLogprobs,
			TopP:              in.TopP,
			Include:           in.Include,
			Metadata:          in.Metadata,
			Stream:            in.Stream,
			Reasoning:         in.Reasoning,
			Text:              in.Text,
		},
	}
}

func (in *Response) ToNativeResponse() *responses2.Response {
	return &in.Response
}

func (in *ResponseChunk) ToNativeResponseChunk() *responses2.ResponseChunk {
	return &in.ResponseChunk
}
