package openai_chat_completion

import (
	chat_completion2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/chat_completion"
)

func NativeRequestToRequest(in *chat_completion2.Request) *Request {
	return &Request{
		*in,
	}
}

func NativeResponseToResponse(in *chat_completion2.Response) *Response {
	return &Response{
		*in,
	}
}
