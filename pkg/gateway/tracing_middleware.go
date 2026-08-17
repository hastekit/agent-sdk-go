package gateway

import (
	"context"
	"strings"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/chat_completion"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/speech"
	"github.com/hastekit/agent-sdk-go/pkg/genai"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// TracingMiddleware emits one OpenTelemetry span per LLM gateway request,
// following the GenAI semantic conventions. It replaces the span code that
// used to live inline in every per-modality handler: it inspects the
// llm.Request to name the span and record request attributes, and the
// llm.Response (or, for streaming, the response chunks) to record the result.
//
// NewLLMGateway installs it by default so tracing is on out of the box.
// Because it is an ordinary gateway Middleware, callers can compose it with
// their own middleware, and it is the single place gateway tracing lives.
type TracingMiddleware struct{}

// NewTracingMiddleware returns the built-in gateway tracing middleware.
func NewTracingMiddleware() *TracingMiddleware { return &TracingMiddleware{} }

var _ Middleware = (*TracingMiddleware)(nil)

func (m *TracingMiddleware) HandleRequest(next RequestHandler) RequestHandler {
	return func(ctx context.Context, providerName llm.ProviderName, key string, r *llm.Request) (*llm.Response, error) {
		ctx, span := startLLMSpan(ctx, providerName, r, false)
		defer span.End()

		resp, err := next(ctx, providerName, key, r)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return resp, err
		}

		setResponseAttributes(span, resp)
		return resp, nil
	}
}

func (m *TracingMiddleware) HandleStreamingRequest(next StreamingRequestHandler) StreamingRequestHandler {
	return func(ctx context.Context, providerName llm.ProviderName, key string, r *llm.Request) (*llm.StreamingResponse, error) {
		ctx, span := startLLMSpan(ctx, providerName, r, true)

		resp, err := next(ctx, providerName, key, r)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			span.End()
			return resp, err
		}

		// The span stays open until the stream drains; wrapStreamingResponse
		// swaps in a channel that observes chunks, records the final usage /
		// output, and ends the span when the provider's channel closes.
		wrapStreamingResponse(span, resp)
		return resp, nil
	}
}

// startLLMSpan opens the request span and records the operation, provider,
// model, request-type, and per-modality request attributes.
func startLLMSpan(ctx context.Context, providerName llm.ProviderName, r *llm.Request, streaming bool) (context.Context, trace.Span) {
	op, reqType := operationAndType(r, streaming)
	model := r.GetRequestedModel()

	ctx, span := tracer.Start(ctx, op+" "+model)
	addToSpan(ctx, span)
	span.SetAttributes(
		attribute.String(genai.AttrOperationName, op),
		attribute.String(genai.AttrProviderName, string(providerName)),
		attribute.String(genai.AttrRequestModel, model),
		attribute.String(genai.AttrRequestType, reqType),
	)
	setRequestAttributes(span, r)
	return ctx, span
}

// operationAndType maps the request modality (and whether it streams) to the
// GenAI operation name and the hastekit request-type label.
func operationAndType(r *llm.Request, streaming bool) (op, reqType string) {
	switch {
	case r.OfResponsesInput != nil:
		if streaming {
			return genai.OpChat, genai.RequestTypeResponsesStream
		}
		return genai.OpChat, genai.RequestTypeResponses
	case r.OfChatCompletionInput != nil:
		if streaming {
			return genai.OpChat, genai.RequestTypeChatStream
		}
		return genai.OpChat, genai.RequestTypeChat
	case r.OfEmbeddingsInput != nil:
		return genai.OpEmbeddings, genai.RequestTypeEmbeddings
	case r.OfSpeech != nil:
		if streaming {
			return genai.OpSpeech, genai.RequestTypeSpeechStream
		}
		return genai.OpSpeech, genai.RequestTypeSpeech
	case r.OfTranscription != nil:
		return genai.OpTranscription, genai.RequestTypeTranscription
	case r.OfImageGeneration != nil:
		return genai.OpImageGeneration, genai.RequestTypeImageGeneration
	case r.OfImageEdit != nil:
		return genai.OpImageEdit, genai.RequestTypeImageEdit
	}
	return genai.OpChat, ""
}

func setRequestAttributes(span trace.Span, r *llm.Request) {
	switch {
	case r.OfResponsesInput != nil:
		in := r.OfResponsesInput
		if in.Temperature != nil {
			span.SetAttributes(attribute.Float64(genai.AttrRequestTemperature, *in.Temperature))
		}
		if in.TopP != nil {
			span.SetAttributes(attribute.Float64(genai.AttrRequestTopP, *in.TopP))
		}
		if in.MaxOutputTokens != nil {
			span.SetAttributes(attribute.Int(genai.AttrRequestMaxTokens, *in.MaxOutputTokens))
		}
		if in.Instructions != nil {
			span.SetAttributes(attribute.String(genai.AttrSystemInstructions, *in.Instructions))
		}
		// Pass &in.Input, not in.Input: InputUnion.MarshalJSON has a pointer
		// receiver, so marshaling the value by copy skips it and leaks the raw
		// union fields (e.g. {"OfInputMessageList":[...]}).
		if s, ok := genai.InputMessages(&in.Input); ok {
			span.SetAttributes(attribute.String(genai.AttrInputMessages, s))
		}

	case r.OfChatCompletionInput != nil:
		in := r.OfChatCompletionInput
		if in.Temperature != nil {
			span.SetAttributes(attribute.Float64(genai.AttrRequestTemperature, *in.Temperature))
		}
		if in.TopP != nil {
			span.SetAttributes(attribute.Float64(genai.AttrRequestTopP, *in.TopP))
		}
		if in.FrequencyPenalty != nil {
			span.SetAttributes(attribute.Float64(genai.AttrRequestFrequencyPenalty, *in.FrequencyPenalty))
		}
		if in.PresencePenalty != nil {
			span.SetAttributes(attribute.Float64(genai.AttrRequestPresencePenalty, *in.PresencePenalty))
		}
		if in.Seed != nil {
			span.SetAttributes(attribute.Int64(genai.AttrRequestSeed, *in.Seed))
		}
		if in.N != nil {
			span.SetAttributes(attribute.Int64(genai.AttrRequestChoiceCount, *in.N))
		}
		if in.MaxCompletionTokens != nil {
			span.SetAttributes(attribute.Int64(genai.AttrRequestMaxTokens, *in.MaxCompletionTokens))
		} else if in.MaxTokens != nil {
			span.SetAttributes(attribute.Int64(genai.AttrRequestMaxTokens, *in.MaxTokens))
		}
		if in.Stop != nil && len(in.Stop.OfList) > 0 {
			span.SetAttributes(attribute.StringSlice(genai.AttrRequestStopSequences, in.Stop.OfList))
		} else if in.Stop != nil && in.Stop.OfString != nil {
			span.SetAttributes(attribute.StringSlice(genai.AttrRequestStopSequences, []string{*in.Stop.OfString}))
		}
		if s, ok := genai.ChatInputMessages(in.Messages); ok {
			span.SetAttributes(attribute.String(genai.AttrInputMessages, s))
		}

	case r.OfEmbeddingsInput != nil:
		in := r.OfEmbeddingsInput
		if in.EncodingFormat != nil {
			span.SetAttributes(attribute.StringSlice(genai.AttrRequestEncodingFormats, []string{*in.EncodingFormat}))
		}
	}
}

func setResponseAttributes(span trace.Span, resp *llm.Response) {
	if resp == nil {
		return
	}
	switch {
	case resp.OfResponsesOutput != nil:
		out := resp.OfResponsesOutput
		span.SetAttributes(attribute.String(genai.AttrResponseModel, out.Model))
		if out.ID != "" {
			span.SetAttributes(attribute.String(genai.AttrResponseID, out.ID))
		}
		if s, ok := genai.OutputMessages(out.Output); ok {
			span.SetAttributes(attribute.String(genai.AttrOutputMessages, s))
		}
		if out.Usage != nil {
			span.SetAttributes(
				attribute.Int(genai.AttrUsageInputTokens, int(out.Usage.InputTokens)),
				attribute.Int(genai.AttrUsageOutputTokens, int(out.Usage.OutputTokens)),
			)
		}

	case resp.OfChatCompletionOutput != nil:
		out := resp.OfChatCompletionOutput
		span.SetAttributes(attribute.String(genai.AttrResponseModel, out.Model))
		if out.ID != "" {
			span.SetAttributes(attribute.String(genai.AttrResponseID, out.ID))
		}
		if len(out.Choices) > 0 {
			finishReasons := make([]string, 0, len(out.Choices))
			for _, c := range out.Choices {
				finishReasons = append(finishReasons, c.FinishReason)
			}
			span.SetAttributes(attribute.StringSlice(genai.AttrFinishReasons, finishReasons))
		}
		if s, ok := genai.ChatChoices(out.Choices); ok {
			span.SetAttributes(attribute.String(genai.AttrOutputMessages, s))
		}
		if out.Usage.TotalTokens > 0 {
			span.SetAttributes(
				attribute.Int64(genai.AttrUsageInputTokens, out.Usage.PromptTokens),
				attribute.Int64(genai.AttrUsageOutputTokens, out.Usage.CompletionTokens),
			)
		}

	case resp.OfEmbeddingsOutput != nil:
		out := resp.OfEmbeddingsOutput
		span.SetAttributes(attribute.String(genai.AttrResponseModel, out.Model))
		if out.Usage != nil {
			// Embeddings only consume input tokens.
			span.SetAttributes(attribute.Int64(genai.AttrUsageInputTokens, out.Usage.PromptTokens))
		}
	}
}

// wrapStreamingResponse replaces the provider's stream channel with one that
// forwards every chunk, records the final usage / output attributes, and ends
// the span when the channel closes. Only the modalities the gateway streams
// (responses, chat completion, speech) are handled; anything else ends the
// span immediately.
func wrapStreamingResponse(span trace.Span, resp *llm.StreamingResponse) {
	switch {
	case resp != nil && resp.ResponsesStreamData != nil:
		orig := resp.ResponsesStreamData
		wrapped := make(chan *responses.ResponseChunk)
		resp.ResponsesStreamData = wrapped
		go func() {
			// Defers run LIFO: end the span, then close the wrapped channel,
			// so a consumer that sees the channel close is guaranteed the span
			// is already recorded.
			defer close(wrapped)
			defer span.End()
			for chunk := range orig {
				wrapped <- chunk
				if chunk.OfResponseCompleted != nil {
					usage := chunk.OfResponseCompleted.Response.Usage
					span.SetAttributes(attribute.Int(genai.AttrUsageInputTokens, usage.InputTokens))
					span.SetAttributes(attribute.Int(genai.AttrCachedInputTokens, usage.InputTokensDetails.CachedTokens))
					span.SetAttributes(attribute.Int(genai.AttrUsageOutputTokens, usage.OutputTokens))
					if s, ok := genai.OutputMessages(chunk.OfResponseCompleted.Response.Output); ok {
						span.SetAttributes(attribute.String(genai.AttrOutputMessages, s))
					}
				}
			}
		}()

	case resp != nil && resp.ChatCompletionStreamData != nil:
		orig := resp.ChatCompletionStreamData
		wrapped := make(chan *chat_completion.ResponseChunk)
		resp.ChatCompletionStreamData = wrapped
		go func() {
			// Defers run LIFO: record the accumulated output, then end the
			// span, then close the wrapped channel.
			defer close(wrapped)
			defer span.End()
			var msgString strings.Builder
			defer func() {
				// Streaming chat accumulates plain text; wrap it as a semconv
				// assistant message.
				if s, ok := genai.AssistantText(msgString.String()); ok {
					span.SetAttributes(attribute.String(genai.AttrOutputMessages, s))
				}
			}()
			for chunk := range orig {
				wrapped <- chunk
				if chunk.OfChatCompletionChunk != nil {
					if chunk.OfChatCompletionChunk.Usage != nil {
						span.SetAttributes(attribute.Int64(genai.AttrUsageInputTokens, chunk.OfChatCompletionChunk.Usage.PromptTokens))
						span.SetAttributes(attribute.Int64(genai.AttrCachedInputTokens, chunk.OfChatCompletionChunk.Usage.PromptTokensDetails.CachedTokens))
						span.SetAttributes(attribute.Int64(genai.AttrUsageOutputTokens, chunk.OfChatCompletionChunk.Usage.CompletionTokens))
					}
					if len(chunk.OfChatCompletionChunk.Choices) > 0 {
						msgString.WriteString(chunk.OfChatCompletionChunk.Choices[0].Delta.Content)
					}
				}
			}
		}()

	case resp != nil && resp.SpeechStreamData != nil:
		orig := resp.SpeechStreamData
		wrapped := make(chan *speech.ResponseChunk)
		resp.SpeechStreamData = wrapped
		go func() {
			defer close(wrapped)
			defer span.End()
			for chunk := range orig {
				wrapped <- chunk
				if chunk.OfAudioDone != nil {
					span.SetAttributes(attribute.Int(genai.AttrUsageInputTokens, chunk.OfAudioDone.Usage.InputTokens))
					span.SetAttributes(attribute.Int(genai.AttrCachedInputTokens, chunk.OfAudioDone.Usage.InputTokensDetails.CachedTokens))
					span.SetAttributes(attribute.Int(genai.AttrUsageOutputTokens, chunk.OfAudioDone.Usage.OutputTokens))
				}
			}
		}()

	default:
		span.End()
	}
}
