package llm

import (
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/chat_completion"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/embeddings"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/image_edit"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/image_generation"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/speech"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/transcription"
)

type Request struct {
	OfEmbeddingsInput     *embeddings.Request
	OfResponsesInput      *responses.Request
	OfChatCompletionInput *chat_completion.Request
	OfSpeech              *speech.Request
	OfTranscription       *transcription.Request
	OfImageGeneration     *image_generation.Request
	OfImageEdit           *image_edit.Request
}

func (r *Request) GetRequestedModel() string {
	if r.OfResponsesInput != nil {
		return r.OfResponsesInput.Model
	}

	if r.OfEmbeddingsInput != nil {
		return r.OfEmbeddingsInput.Model
	}

	if r.OfChatCompletionInput != nil {
		return r.OfChatCompletionInput.Model
	}

	if r.OfSpeech != nil {
		return r.OfSpeech.Model
	}

	if r.OfTranscription != nil {
		return r.OfTranscription.Model
	}

	if r.OfImageGeneration != nil {
		return r.OfImageGeneration.Model
	}

	if r.OfImageEdit != nil {
		return r.OfImageEdit.Model
	}

	return ""
}

// SetRequestedModel points the request at a different model, whichever
// variant it carries. It is the counterpart to GetRequestedModel.
//
// An empty model is ignored: a caller redirecting a request to another
// provider without naming a model keeps the one already set, and blanking it
// is never what was meant.
//
// This mutates. To retarget a request another caller still holds — a fallback
// chain reissuing one — set the model on a Clone.
func (r *Request) SetRequestedModel(model string) {
	if r == nil || model == "" {
		return
	}

	switch {
	case r.OfResponsesInput != nil:
		r.OfResponsesInput.Model = model
	case r.OfEmbeddingsInput != nil:
		r.OfEmbeddingsInput.Model = model
	case r.OfChatCompletionInput != nil:
		r.OfChatCompletionInput.Model = model
	case r.OfSpeech != nil:
		r.OfSpeech.Model = model
	case r.OfTranscription != nil:
		r.OfTranscription.Model = model
	case r.OfImageGeneration != nil:
		r.OfImageGeneration.Model = model
	case r.OfImageEdit != nil:
		r.OfImageEdit.Model = model
	}
}

// Clone copies the request far enough that SetRequestedModel on the result
// leaves the original alone: the union and the one variant it carries are
// both copied, because the variants are pointers and writing the model
// through a shared one would rewrite the caller's request.
//
// Everything below that — messages, tools, parameters — is shared. This is a
// copy you can retarget, not a deep copy, and a caller that mutates the
// contents of one still affects the other.
func (r *Request) Clone() *Request {
	if r == nil {
		return nil
	}

	out := *r
	switch {
	case r.OfResponsesInput != nil:
		in := *r.OfResponsesInput
		out.OfResponsesInput = &in
	case r.OfEmbeddingsInput != nil:
		in := *r.OfEmbeddingsInput
		out.OfEmbeddingsInput = &in
	case r.OfChatCompletionInput != nil:
		in := *r.OfChatCompletionInput
		out.OfChatCompletionInput = &in
	case r.OfSpeech != nil:
		in := *r.OfSpeech
		out.OfSpeech = &in
	case r.OfTranscription != nil:
		in := *r.OfTranscription
		out.OfTranscription = &in
	case r.OfImageGeneration != nil:
		in := *r.OfImageGeneration
		out.OfImageGeneration = &in
	case r.OfImageEdit != nil:
		in := *r.OfImageEdit
		out.OfImageEdit = &in
	}
	return &out
}

type Response struct {
	OfEmbeddingsOutput     *embeddings.Response
	OfResponsesOutput      *responses.Response
	OfChatCompletionOutput *chat_completion.Response
	OfSpeech               *speech.Response
	OfTranscription        *transcription.Response
	OfImageGeneration      *image_generation.Response
	OfImageEdit            *image_edit.Response
	Error                  *Error
}

type StreamingResponse struct {
	ResponsesStreamData      chan *responses.ResponseChunk
	ChatCompletionStreamData chan *chat_completion.ResponseChunk
	SpeechStreamData         chan *speech.ResponseChunk
}

type Error struct {
	Message string
}
