package xai_responses

import (
	responses2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/openai/openai_responses"
)

func NativeRequestToRequest(in *responses2.Request) *Request {
	r := &Request{
		Request: &openai_responses.Request{
			Request: *in,
		},
	}

	// Grok doesn't support reasoning effort except for older models like grok-3
	if in.Reasoning != nil {
		r.Reasoning.Effort = nil
	}

	// Filter out image generation tools as xAI doesn't support image generation as a tool.
	// xAI supports image generation via a separate api.
	tools := []responses2.ToolUnion{}
	for _, tool := range in.Tools {
		if tool.OfImageGeneration != nil {
			continue
		}
		tools = append(tools, tool)
	}
	r.Tools = tools

	return r
}

func NativeResponseToResponse(in *responses2.Response) *Response {
	return &Response{
		Response: *in,
	}
}
