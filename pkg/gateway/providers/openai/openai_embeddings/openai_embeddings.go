package openai_embeddings

import (
	embeddings2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/embeddings"
)

type Request struct {
	embeddings2.Request
}

func (r *Request) ToNativeRequest() *embeddings2.Request {
	return &r.Request
}

func NativeRequestToRequest(in *embeddings2.Request) *Request {
	return &Request{*in}
}

type Response struct {
	embeddings2.Response
}

func (r *Response) ToNativeResponse() *embeddings2.Response {
	return &r.Response
}

func NativeResponseToResponse(in *embeddings2.Response) *Response {
	return &Response{*in}
}
