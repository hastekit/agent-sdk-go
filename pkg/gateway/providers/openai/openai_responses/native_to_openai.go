package openai_responses

import (
	responses2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

func NativeRequestToRequest(in *responses2.Request) *Request {
	req := &Request{
		*in,
	}

	req.ExtraFields = nil

	return req
}

func NativeResponseToResponse(in *responses2.Response) *Response {
	return &Response{
		*in,
	}
}
