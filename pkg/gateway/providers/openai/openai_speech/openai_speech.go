package openai_speech

import (
	speech2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/speech"
)

type Request struct {
	speech2.Request
}

func (r *Request) ToNativeRequest() *speech2.Request {
	return &r.Request
}

func NativeRequestToRequest(in *speech2.Request) *Request {
	return &Request{*in}
}

type Response struct {
	speech2.Response
}

func (r *Response) ToNativeResponse() *speech2.Response {
	return &r.Response
}

func NativeResponseToResponse(in *speech2.Response) *Response {
	return &Response{*in}
}

type ResponseChunk struct {
	speech2.ResponseChunk
}

func (r *ResponseChunk) ToNativeResponse() *speech2.ResponseChunk {
	return &r.ResponseChunk
}
