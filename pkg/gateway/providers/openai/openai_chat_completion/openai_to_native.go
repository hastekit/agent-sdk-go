package openai_chat_completion

import (
	chat_completion2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/chat_completion"
)

func (in *Request) ToNativeRequest() *chat_completion2.Request {
	return &in.Request
}

func (in *Response) ToNativeResponse() *chat_completion2.Response {
	return &in.Response
}
