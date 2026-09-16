package agents

import (
	"context"
	"io"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// LLM is the loop's view of the model: one streamed call per iteration. call
// is the ModelCall the request belongs to, for a runtime that runs the middlewares'
// WrapModelCall on the far side of a boundary and has to hand it to them.
type LLM interface {
	NewStreamingResponses(ctx context.Context, call *ModelCall, in *responses.Request, cb func(chunk *responses.ResponseChunk)) (*responses.Response, error)
}

type WrappedLLM struct {
	llm llm.Provider
}

func (l *WrappedLLM) NewStreamingResponses(ctx context.Context, call *ModelCall, in *responses.Request, cb func(chunk *responses.ResponseChunk)) (*responses.Response, error) {
	response, err := InvokeModelCall(ctx, l.llm, call, in, cb)
	if err != nil && ctx.Err() != nil {
		return nil, ErrModelCallStopped
	}
	return response, err
}

type Accumulator struct {
}

// ReadStream folds a provider's chunk stream into one response, publishing
// each chunk as it arrives.
//
// It gives up when ctx is cancelled — which is how a stop ends a model call
// mid-stream — and reports ErrModelCallStopped. Whatever had accumulated is
// dropped rather than returned: a stopped run does not answer, and the loop
// writes its cancellation notice instead. The unread remainder is drained in
// the background so the provider's sender is never left blocked on a channel
// nobody is reading.
func (a *Accumulator) ReadStream(ctx context.Context, stream chan *responses.ResponseChunk, cb func(chunk *responses.ResponseChunk)) (*responses.Response, error) {
	return a.readStream(ctx, stream, func(chunk *responses.ResponseChunk) error {
		cb(chunk)
		return nil
	})
}

// readStream is the error-aware form used by model invocation. Keeping it
// internal preserves the public callback API while allowing middleware stream
// transforms to fail before a chunk is published.
func (a *Accumulator) readStream(ctx context.Context, stream chan *responses.ResponseChunk, cb func(chunk *responses.ResponseChunk) error) (*responses.Response, error) {
	// Process stream
	finalOutput := []responses.OutputMessageUnion{}
	var usage *responses.Usage
	completed := false
	for {
		var chunk *responses.ResponseChunk
		var open bool

		select {
		case chunk, open = <-stream:
			if !open {
				// A provider that honours cancellation closes its stream, so
				// the end of the channel is ambiguous: it means either the
				// model finished or the stop reached it first. The context is
				// what tells them apart — without this check the outcome would
				// depend on which of the two raced ahead, and a stopped run
				// would sometimes be recorded as a complete answer.
				if ctx.Err() != nil {
					return nil, ErrModelCallStopped
				}
				if !completed {
					return nil, io.ErrUnexpectedEOF
				}
				return &responses.Response{Output: finalOutput, Usage: usage}, nil
			}
		case <-ctx.Done():
			go drain(stream)
			return nil, ErrModelCallStopped
		}

		if chunk == nil {
			continue
		}
		if chunk.OfError != nil {
			go drain(stream)
			return nil, chunk.OfError
		}
		if err := cb(chunk); err != nil {
			go drain(stream)
			return nil, err
		}
		switch chunk.ChunkType() {
		case "response.output_item.done":
			if chunk.OfOutputItemDone.Item.Type == "message" {
				var contentItems responses.ChunkOutputItemContent
				if chunk.OfOutputItemDone.Item.Content != nil {
					contentItems = *chunk.OfOutputItemDone.Item.Content
				}
				for _, content := range contentItems {
					if content.OfOutputText != nil {
						finalOutput = append(finalOutput, responses.OutputMessageUnion{
							OfOutputMessage: &responses.OutputMessage{
								ID:   chunk.OfOutputItemDone.Item.Id,
								Role: constants.RoleAssistant,
								Content: &responses.OutputContent{
									{OfOutputText: content.OfOutputText},
								},
							},
						})
					}
				}
			}

			if chunk.OfOutputItemDone.Item.Type == "reasoning" {
				// reasoning_text (only OSS)
				// We unify `reasoning_text` into `summary_text` for simplicity
				if chunk.OfOutputItemDone.Item.Content != nil {
					for _, content := range *chunk.OfOutputItemDone.Item.Content {
						if content.OfReasoningText != nil {
							finalOutput = append(finalOutput, responses.OutputMessageUnion{
								OfReasoning: &responses.ReasoningMessage{
									ID: chunk.OfOutputItemDone.Item.Id,
									Summary: []responses.SummaryTextContent{
										{
											Text: content.OfReasoningText.Text,
										},
									},
								},
							})
						}
					}
				}

				// Skip empty reasoning blocks
				var summary []responses.SummaryTextContent
				if chunk.OfOutputItemDone.Item.Summary != nil {
					summary = *chunk.OfOutputItemDone.Item.Summary
				}
				if chunk.OfOutputItemDone.Item.EncryptedContent == nil && len(summary) == 0 {
					continue
				}

				var encryptedContent *string
				if chunk.OfOutputItemDone.Item.EncryptedContent != nil {
					encryptedContent = chunk.OfOutputItemDone.Item.EncryptedContent
				}

				finalOutput = append(finalOutput, responses.OutputMessageUnion{
					OfReasoning: &responses.ReasoningMessage{
						ID:               chunk.OfOutputItemDone.Item.Id,
						Summary:          summary,
						EncryptedContent: encryptedContent,
					},
				})
			}

			if chunk.OfOutputItemDone.Item.Type == "function_call" {
				finalOutput = append(finalOutput, responses.OutputMessageUnion{
					OfFunctionCall: &responses.FunctionCallMessage{
						ID:               chunk.OfOutputItemDone.Item.Id,
						CallID:           *chunk.OfOutputItemDone.Item.CallID,
						Name:             *chunk.OfOutputItemDone.Item.Name,
						Arguments:        *chunk.OfOutputItemDone.Item.Arguments,
						ThoughtSignature: chunk.OfOutputItemDone.Item.ThoughtSignature,
					},
				})
			}

			if chunk.OfOutputItemDone.Item.Type == "image_generation_call" {
				finalOutput = append(finalOutput, responses.OutputMessageUnion{
					OfImageGenerationCall: &responses.ImageGenerationCallMessage{
						ID:           chunk.OfOutputItemDone.Item.Id,
						Status:       chunk.OfOutputItemDone.Item.Status,
						Background:   stringValue(chunk.OfOutputItemDone.Item.Background),
						OutputFormat: stringValue(chunk.OfOutputItemDone.Item.OutputFormat),
						Quality:      stringValue(chunk.OfOutputItemDone.Item.Quality),
						Size:         stringValue(chunk.OfOutputItemDone.Item.Size),
						Result:       stringValue(chunk.OfOutputItemDone.Item.Result),
					},
				})
			}

		case "response.completed":
			completed = true
			usage = &chunk.OfResponseCompleted.Response.Usage
			// response.completed is the authoritative final response for
			// providers that populate it. Reconcile it with output_item.done
			// instead of appending it: otherwise generated images are either
			// duplicated or missed when only one of the two events has Result.
			finalOutput = reconcileCompletedOutput(finalOutput, chunk.OfResponseCompleted.Response.Output)
		}
	}
}

func stringValue(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// reconcileCompletedOutput follows response.completed ordering, fills an
// incomplete generated-image item from its matching output_item.done, and
// retains done-only items for providers that omit part of completed.output.
func reconcileCompletedOutput(done, completed []responses.OutputMessageUnion) []responses.OutputMessageUnion {
	if len(completed) == 0 {
		return done
	}
	out := append([]responses.OutputMessageUnion(nil), completed...)
	positions := make(map[string]int, len(out))
	for i := range out {
		if key := outputIdentity(out[i]); key != "" {
			positions[key] = i
		}
	}
	for _, item := range done {
		key := outputIdentity(item)
		if i, ok := positions[key]; ok && key != "" {
			out[i] = mergeCompletedImage(item, out[i])
			continue
		}
		out = append(out, item)
	}
	return out
}

func outputIdentity(item responses.OutputMessageUnion) string {
	switch {
	case item.OfOutputMessage != nil && item.OfOutputMessage.ID != "":
		return "message:" + item.OfOutputMessage.ID
	case item.OfFunctionCall != nil && item.OfFunctionCall.ID != "":
		return "function_call:" + item.OfFunctionCall.ID
	case item.OfReasoning != nil && item.OfReasoning.ID != "":
		return "reasoning:" + item.OfReasoning.ID
	case item.OfImageGenerationCall != nil && item.OfImageGenerationCall.ID != "":
		return "image_generation_call:" + item.OfImageGenerationCall.ID
	case item.OfWebSearchCall != nil && item.OfWebSearchCall.ID != "":
		return "web_search_call:" + item.OfWebSearchCall.ID
	case item.OfCodeInterpreterCall != nil && item.OfCodeInterpreterCall.ID != "":
		return "code_interpreter_call:" + item.OfCodeInterpreterCall.ID
	default:
		return ""
	}
}

func mergeCompletedImage(done, completed responses.OutputMessageUnion) responses.OutputMessageUnion {
	if done.OfImageGenerationCall == nil || completed.OfImageGenerationCall == nil {
		return completed
	}
	d, c := done.OfImageGenerationCall, *completed.OfImageGenerationCall
	if c.Status == "" {
		c.Status = d.Status
	}
	if c.Background == "" {
		c.Background = d.Background
	}
	if c.OutputFormat == "" {
		c.OutputFormat = d.OutputFormat
	}
	if c.Quality == "" {
		c.Quality = d.Quality
	}
	if c.Size == "" {
		c.Size = d.Size
	}
	if c.Result == "" {
		c.Result = d.Result
	}
	completed.OfImageGenerationCall = &c
	return completed
}

// drain reads a stream to its end and discards it, so a provider still writing
// into a channel nobody reads is released rather than blocked forever.
func drain(stream chan *responses.ResponseChunk) {
	for range stream {
	}
}
