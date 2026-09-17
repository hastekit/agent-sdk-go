package middleware_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	agentmiddleware "github.com/hastekit/agent-sdk-go/pkg/agents/middleware"
	"github.com/hastekit/agent-sdk-go/pkg/attachments"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	anthropic "github.com/hastekit/agent-sdk-go/pkg/gateway/providers/anthropic/anthropic_responses"
	bedrock "github.com/hastekit/agent-sdk-go/pkg/gateway/providers/bedrock/bedrock_responses"
	gemini "github.com/hastekit/agent-sdk-go/pkg/gateway/providers/gemini/gemini_responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/require"
)

// attachmentResult is a tool result carrying an image and a document inline,
// the shape the middleware exists to keep out of history.
func attachmentResult() *agents.ToolCallResponse {
	image := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a6xkAAAAASUVORK5CYII="
	pdf := base64.StdEncoding.EncodeToString([]byte("%PDF-1.7\nexample\n%%EOF"))
	return &agents.ToolCallResponse{FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
		ID: "result", CallID: "call", Output: responses.FunctionCallOutputContentUnion{OfList: responses.InputContent{
			{OfInputText: &responses.InputTextContent{Text: "Two files"}},
			{OfInputImage: &responses.InputImageContent{ImageURL: &image, Detail: "high"}},
			{OfInputFile: &responses.InputFileContent{FileData: &pdf, FileName: utils.Ptr("report.pdf")}},
		}},
	}, StateUpdates: map[string]string{"status": "ready"}}
}

// externalized runs result through the middleware's wrap as if a tool had returned
// it, and reports what the wrap made of it.
func externalized(middleware agents.ToolCallMiddleware, ctx context.Context, result *agents.ToolCallResponse) (*agents.ToolCallResponse, error) {
	return middleware.WrapToolCall(func(context.Context, *agents.BaseTool, *agents.ToolCall) (*agents.ToolCallResponse, error) {
		return result, nil
	})(ctx, nil, &agents.ToolCall{Namespace: "test"})
}

// prepared runs request through the middleware's wrap and reports what the wrap
// handed down, without any provider behind it.
func prepared(t *testing.T, middleware agents.ModelCallMiddleware, ctx context.Context, request *responses.Request) (*responses.Request, error) {
	t.Helper()
	var got *responses.Request
	_, err := middleware.WrapModelCall(func(_ context.Context, _ *agents.ModelCall, req *responses.Request) (*responses.Response, error) {
		got = req
		return &responses.Response{}, nil
	})(ctx, &agents.ModelCall{Namespace: "test"}, request)
	return got, err
}

// processedModelResponse runs a completed provider response back through a
// model middleware wrap, the point where generated output must become durable.
func processedModelResponse(middleware agents.ModelCallMiddleware, ctx context.Context, response *responses.Response) (*responses.Response, error) {
	return middleware.WrapModelCall(func(context.Context, *agents.ModelCall, *responses.Request) (*responses.Response, error) {
		return response, nil
	})(ctx, &agents.ModelCall{Namespace: "test"}, &responses.Request{})
}

type generatedStreamProvider struct {
	llm.Provider
	chunks []*responses.ResponseChunk
	calls  int
}

func (p *generatedStreamProvider) NewStreamingResponses(context.Context, *responses.Request) (chan *responses.ResponseChunk, error) {
	p.calls++
	stream := make(chan *responses.ResponseChunk, len(p.chunks))
	for _, chunk := range p.chunks {
		stream <- chunk
	}
	close(stream)
	return stream, nil
}

func executeGeneratedStream(ctx context.Context, middlewares []agents.ModelCallMiddleware, provider llm.Provider, published *[]*responses.ResponseChunk) (*responses.Response, error) {
	return agents.ExecuteModelCallWithMiddleware(ctx, middlewares, &agents.ModelCall{Namespace: "test"}, &responses.Request{}, func(ctx context.Context, call *agents.ModelCall, request *responses.Request) (*responses.Response, error) {
		return agents.InvokeModelCall(ctx, provider, call, request, func(chunk *responses.ResponseChunk) {
			*published = append(*published, chunk)
		})
	})
}

func middlewareStore(t *testing.T) *attachments.FileStore {
	t.Helper()
	store, err := attachments.NewFileStore(t.TempDir(), attachments.FileStoreConfig{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// pngBytes is the 1x1 PNG attachmentResult carries as a data URI, decoded so
// it can be uploaded.
func pngBytes(t *testing.T) []byte {
	t.Helper()
	uri := *attachmentResult().Output.OfList[1].OfInputImage.ImageURL
	data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(uri, "data:image/png;base64,"))
	require.NoError(t, err)
	return data
}

// uploadedRefs puts an image and a document in the store and returns their
// references, which is the shape everything durable holds them in.
func uploadedRefs(t *testing.T, store attachments.UploadStore) (image, file attachments.Ref) {
	t.Helper()
	image, err := store.Put(t.Context(), "test", attachments.Upload{Filename: "pixel.png", MediaType: "image/png", Content: bytes.NewReader(pngBytes(t))})
	require.NoError(t, err)
	file, err = store.Put(t.Context(), "test", attachments.Upload{Filename: "report.pdf", MediaType: "application/pdf", Content: strings.NewReader("%PDF-1.7\nexample\n%%EOF")})
	require.NoError(t, err)
	return image, file
}

// referencedRequest is a request as history shapes it: a user message and a
// tool result, each carrying an attachment file_id and no bytes.
func referencedRequest(image, file attachments.Ref) *responses.Request {
	return &responses.Request{Input: responses.InputUnion{OfInputMessageList: []responses.InputMessageUnion{
		{OfInputMessage: &responses.InputMessage{Role: constants.RoleUser, Content: responses.InputContent{
			{OfInputText: &responses.InputTextContent{Text: "What is this?"}},
			{OfInputImage: &responses.InputImageContent{FileID: utils.Ptr(attachments.FileID(image))}},
		}}},
		{OfFunctionCallOutput: &responses.FunctionCallOutputMessage{ID: "out", CallID: "call", Output: responses.FunctionCallOutputContentUnion{OfList: responses.InputContent{
			{OfInputFile: &responses.InputFileContent{FileID: utils.Ptr(attachments.FileID(file))}},
		}}}},
	}}}
}

// A tool's inline result comes out as references, on a copy, and those
// references resolve back to the very bytes the tool returned.
func TestAttachmentMiddlewareRoundTrip(t *testing.T) {
	store := middlewareStore(t)
	middleware := agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{Store: store})
	original := attachmentResult()
	before, err := json.Marshal(original)
	require.NoError(t, err)
	changed, err := externalized(middleware, t.Context(), original)
	require.NoError(t, err)
	require.NotSame(t, original, changed)
	result := changed
	require.Equal(t, original.StateUpdates, result.StateUpdates)
	require.Equal(t, "call", result.CallID)
	require.Equal(t, "Two files", result.Output.OfList[0].OfInputText.Text)
	require.Equal(t, "high", result.Output.OfList[1].OfInputImage.Detail)
	msgs := []responses.InputMessageUnion{{OfFunctionCallOutput: result.FunctionCallOutputMessage}}
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "base64")
	require.Contains(t, string(encoded), "attachment://")
	require.Contains(t, string(encoded), "file_id")
	require.NotContains(t, string(encoded), "file_ref", "messages keep the shared Responses schema")
	after, err := json.Marshal(original)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after), "middleware must not mutate the tool's result")
	hydrated, err := agentmiddleware.PrepareAttachments(t.Context(), "test", &responses.Request{Input: responses.InputUnion{OfInputMessageList: msgs}}, attachments.NewResolver(store, attachments.Config{}), 0)
	require.NoError(t, err)
	content := hydrated.Input.OfInputMessageList[0].OfFunctionCallOutput.Output.OfList
	require.Equal(t, *original.Output.OfList[1].OfInputImage.ImageURL, *content[1].OfInputImage.ImageURL)
	require.True(t, strings.HasPrefix(*content[2].OfInputFile.FileData, "data:application/pdf;base64,"))
	again, err := externalized(middleware, t.Context(), result)
	require.NoError(t, err)
	require.Same(t, result, again)
}

type rejectedUpload struct{ attachments.UploadStore }

func (rejectedUpload) Put(context.Context, string, attachments.Upload) (attachments.Ref, error) {
	return attachments.Ref{}, attachments.ErrDenied
}

// Anything the middleware cannot store ends the run rather than reaching history
// inline; what is not an attachment at all passes through untouched.
func TestAttachmentMiddlewareFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    agentmiddleware.AttachmentMiddlewareConfig
		source string
		want   error
	}{
		{"missing store", agentmiddleware.AttachmentMiddlewareConfig{}, "data:image/png;base64,AAAA", attachments.ErrUnresolved},
		{"invalid base64", agentmiddleware.AttachmentMiddlewareConfig{Store: middlewareStore(t)}, "data:image/png;base64,!!!!", attachments.ErrInvalid},
		{"external URL", agentmiddleware.AttachmentMiddlewareConfig{Store: middlewareStore(t)}, "https://example.com/image.png", attachments.ErrInvalid},
		{"local path", agentmiddleware.AttachmentMiddlewareConfig{Store: middlewareStore(t)}, "/private/image.png", attachments.ErrInvalid},
		{"too large", agentmiddleware.AttachmentMiddlewareConfig{Store: middlewareStore(t), MaxFileBytes: 1}, *attachmentResult().Output.OfList[1].OfInputImage.ImageURL, attachments.ErrTooLarge},
		{"denied", agentmiddleware.AttachmentMiddlewareConfig{Store: rejectedUpload{}}, *attachmentResult().Output.OfList[1].OfInputImage.ImageURL, attachments.ErrDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := attachmentResult()
			result.Output.OfList[1].OfInputImage.ImageURL = &tc.source
			call := &agents.ToolCall{FunctionCallMessage: &responses.FunctionCallMessage{ID: "id", CallID: "call", Name: "media"}, Namespace: "test"}
			out, err := agents.ExecuteToolWithMiddleware(t.Context(), []agents.ToolCallMiddleware{agentmiddleware.NewAttachmentMiddleware(tc.cfg)}, agents.ExecutableToolCall{ToolCall: call}, func(context.Context, *agents.ToolCall) (*agents.ToolCallResponse, error) { return result, nil })
			require.Nil(t, out)
			require.True(t, agents.IsToolCallAborted(err))
			require.True(t, errors.Is(err, tc.want), err)
		})
	}
	middleware := agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{})
	plain := agents.ToolCallResult(&agents.ToolCall{FunctionCallMessage: &responses.FunctionCallMessage{}}, `{"image":"not a structured attachment"}`)
	change, err := externalized(middleware, t.Context(), plain)
	require.NoError(t, err)
	require.Same(t, plain, change)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = externalized(agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{Store: middlewareStore(t)}), ctx, attachmentResult())
	require.ErrorIs(t, err, context.Canceled)
}

type countingUploads struct {
	attachments.UploadStore
	puts int
}

func (s *countingUploads) Put(ctx context.Context, namespace string, in attachments.Upload) (attachments.Ref, error) {
	s.puts++
	return s.UploadStore.Put(ctx, namespace, in)
}

// The same bytes under the same name within one result are stored once.
func TestAttachmentMiddlewareDeduplicatesWithinResult(t *testing.T) {
	store := &countingUploads{UploadStore: middlewareStore(t)}
	result := attachmentResult()
	result.Output.OfList = append(result.Output.OfList, result.Output.OfList[1])
	middleware := agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{Store: store})
	out, err := externalized(middleware, t.Context(), result)
	require.NoError(t, err)
	require.Equal(t, 2, store.puts)
	require.Equal(t, out.Output.OfList[1].OfInputImage.FileID, out.Output.OfList[3].OfInputImage.FileID)
}

// Generated image bytes are uploaded before the response leaves model
// middleware. The response and image item are copied, metadata is preserved,
// duplicate payloads share a reference, and already-owned results stay owned.
func TestAttachmentMiddlewareExternalizesGeneratedImages(t *testing.T) {
	base := middlewareStore(t)
	owned, err := base.Put(t.Context(), "test", attachments.Upload{
		Filename: "owned.png", MediaType: "image/png", Content: bytes.NewReader(pngBytes(t)),
	})
	require.NoError(t, err)
	store := &countingUploads{UploadStore: base}
	raw := base64.StdEncoding.EncodeToString(pngBytes(t))
	original := &responses.Response{
		ID: "resp_1", Model: "image-model", Metadata: map[string]any{"trace": "kept"},
		Output: []responses.OutputMessageUnion{
			{OfImageGenerationCall: &responses.ImageGenerationCallMessage{
				ID: "ig_1", Status: "completed", Background: "opaque", OutputFormat: "png",
				Quality: "high", Size: "1024x1024", Result: raw,
			}},
			{OfImageGenerationCall: &responses.ImageGenerationCallMessage{
				ID: "ig_2", Status: "completed", Result: raw,
			}},
			{OfImageGenerationCall: &responses.ImageGenerationCallMessage{
				ID: "ig_3", Status: "completed", OutputFormat: "png", Result: attachments.FileID(owned),
			}},
		},
	}
	before, err := json.Marshal(original)
	require.NoError(t, err)

	got, err := processedModelResponse(agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{Store: store}), t.Context(), original)
	require.NoError(t, err)
	require.NotSame(t, original, got)
	require.Equal(t, 1, store.puts, "duplicate generated bytes must be uploaded once")
	require.Equal(t, got.Output[0].OfImageGenerationCall.Result, got.Output[1].OfImageGenerationCall.Result)
	require.Equal(t, attachments.FileID(owned), got.Output[2].OfImageGenerationCall.Result)
	require.True(t, attachments.IsFileID(got.Output[0].OfImageGenerationCall.Result))
	require.Equal(t, "ig_1", got.Output[0].OfImageGenerationCall.ID)
	require.Equal(t, "completed", got.Output[0].OfImageGenerationCall.Status)
	require.Equal(t, "opaque", got.Output[0].OfImageGenerationCall.Background)
	require.Equal(t, "png", got.Output[0].OfImageGenerationCall.OutputFormat)
	require.Equal(t, "high", got.Output[0].OfImageGenerationCall.Quality)
	require.Equal(t, "1024x1024", got.Output[0].OfImageGenerationCall.Size)
	require.Equal(t, "resp_1", got.ID)
	require.Equal(t, "image-model", got.Model)
	require.Equal(t, "kept", got.Metadata["trace"])

	ref, err := attachments.RefFromFileID(got.Output[0].OfImageGenerationCall.Result)
	require.NoError(t, err)
	blob, err := attachments.NewResolver(base, attachments.Config{}).Resolve(t.Context(), "test", ref)
	require.NoError(t, err)
	var stored bytes.Buffer
	_, err = blob.WriteTo(&stored)
	require.NoError(t, err)
	require.Equal(t, pngBytes(t), stored.Bytes(), "the original decoded bytes are stored without conversion")

	after, err := json.Marshal(original)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after), "the provider response must remain unchanged")
	encoded, err := json.Marshal(got)
	require.NoError(t, err)
	require.Contains(t, string(encoded), "attachment://")
	require.NotContains(t, string(encoded), raw)
	require.NotContains(t, string(encoded), "file_ref", "generated images retain the shared Responses shape")
}

func TestAttachmentMiddlewareExternalizesGeneratedImageStreamBeforePublishing(t *testing.T) {
	store := &countingUploads{UploadStore: middlewareStore(t)}
	raw := base64.StdEncoding.EncodeToString(pngBytes(t))
	done := &responses.ResponseChunk{OfOutputItemDone: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{
		Item: responses.ChunkOutputItemData{
			Type: "image_generation_call", Id: "ig_stream", Status: "completed",
			OutputFormat: utils.Ptr("png"), Quality: utils.Ptr("high"), Result: &raw,
		},
	}}
	completedImage := responses.OutputMessageUnion{OfImageGenerationCall: &responses.ImageGenerationCallMessage{
		ID: "ig_stream", Status: "completed", OutputFormat: "png", Quality: "high", Result: raw,
	}}
	completed := &responses.ResponseChunk{OfResponseCompleted: &responses.ChunkResponse[constants.ChunkTypeResponseCompleted]{
		Response: responses.ChunkResponseData{Output: []responses.OutputMessageUnion{completedImage}},
	}}
	created := &responses.ResponseChunk{OfResponseCreated: &responses.ChunkResponse[constants.ChunkTypeResponseCreated]{}}
	progress := &responses.ResponseChunk{OfImageGenerationCallInProgress: &responses.ChunkImageGenerationCall[constants.ChunkTypeImageGenerationCallInProgress]{ItemId: "ig_stream"}}
	partial := &responses.ResponseChunk{OfImageGenerationCallPartialImage: &responses.ChunkImageGenerationCall[constants.ChunkTypeImageGenerationCallPartialImage]{
		ItemId: "ig_stream", PartialImageIndex: 0, PartialImageBase64: "binary-preview",
	}}
	textDelta := &responses.ResponseChunk{OfOutputTextDelta: &responses.ChunkOutputText[constants.ChunkTypeOutputTextDelta]{ItemId: "msg_1", Delta: "still streaming"}}
	toolDelta := &responses.ResponseChunk{OfFunctionCallArgumentsDelta: &responses.ChunkFunctionCall[constants.ChunkTypeFunctionCallArgumentsDelta]{ItemId: "fc_1", Delta: "{}"}}
	provider := &generatedStreamProvider{chunks: []*responses.ResponseChunk{created, progress, partial, textDelta, toolDelta, done, completed}}
	sourceBefore, err := json.Marshal(provider.chunks)
	require.NoError(t, err)

	var published []*responses.ResponseChunk
	response, err := executeGeneratedStream(t.Context(), []agents.ModelCallMiddleware{
		agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{Store: store}),
	}, provider, &published)
	require.NoError(t, err)
	require.Equal(t, 1, store.puts, "done, completed, and accumulated output share one upload")
	require.Len(t, published, 6, "the binary partial preview is suppressed but progress/text/tool events remain")
	require.Same(t, created, published[0], "unrelated streaming events pass through unchanged")
	require.Same(t, progress, published[1])
	require.Same(t, textDelta, published[2])
	require.Same(t, toolDelta, published[3])
	require.NotNil(t, published[4].OfOutputItemDone)
	require.NotNil(t, published[5].OfResponseCompleted)
	doneRef := *published[4].OfOutputItemDone.Item.Result
	completedRef := published[5].OfResponseCompleted.Response.Output[0].OfImageGenerationCall.Result
	require.True(t, attachments.IsFileID(doneRef))
	require.Equal(t, doneRef, completedRef)
	require.Len(t, response.Output, 1)
	require.Equal(t, doneRef, response.Output[0].OfImageGenerationCall.Result)
	for _, chunk := range published {
		wire, marshalErr := json.Marshal(chunk)
		require.NoError(t, marshalErr)
		require.NotContains(t, string(wire), raw)
		require.NotContains(t, string(wire), "binary-preview")
	}

	sourceAfter, err := json.Marshal(provider.chunks)
	require.NoError(t, err)
	require.Equal(t, string(sourceBefore), string(sourceAfter), "provider-owned chunks must not be mutated")
	require.Equal(t, raw, *done.OfOutputItemDone.Item.Result)
	require.Equal(t, raw, completed.OfResponseCompleted.Response.Output[0].OfImageGenerationCall.Result)
}

func TestGeneratedImageStreamingIsUnchangedWithoutAttachmentMiddleware(t *testing.T) {
	raw := base64.StdEncoding.EncodeToString(pngBytes(t))
	partial := &responses.ResponseChunk{OfImageGenerationCallPartialImage: &responses.ChunkImageGenerationCall[constants.ChunkTypeImageGenerationCallPartialImage]{PartialImageBase64: "preview"}}
	done := &responses.ResponseChunk{OfOutputItemDone: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{Item: responses.ChunkOutputItemData{
		Type: "image_generation_call", Id: "ig_raw", OutputFormat: utils.Ptr("png"), Result: &raw,
	}}}
	completed := &responses.ResponseChunk{OfResponseCompleted: &responses.ChunkResponse[constants.ChunkTypeResponseCompleted]{}}
	provider := &generatedStreamProvider{chunks: []*responses.ResponseChunk{partial, done, completed}}
	var published []*responses.ResponseChunk

	response, err := executeGeneratedStream(t.Context(), nil, provider, &published)
	require.NoError(t, err)
	require.Equal(t, []*responses.ResponseChunk{partial, done, completed}, published)
	require.Equal(t, raw, response.Output[0].OfImageGenerationCall.Result)
}

func TestAttachmentMiddlewareExternalizesCompletedOnlyGeneratedImageStream(t *testing.T) {
	store := &countingUploads{UploadStore: middlewareStore(t)}
	raw := base64.StdEncoding.EncodeToString(pngBytes(t))
	completed := &responses.ResponseChunk{OfResponseCompleted: &responses.ChunkResponse[constants.ChunkTypeResponseCompleted]{
		Response: responses.ChunkResponseData{Output: []responses.OutputMessageUnion{{
			OfImageGenerationCall: &responses.ImageGenerationCallMessage{ID: "ig_completed_only", OutputFormat: "png", Result: raw},
		}}},
	}}
	provider := &generatedStreamProvider{chunks: []*responses.ResponseChunk{
		{OfImageGenerationCallPartialImage: &responses.ChunkImageGenerationCall[constants.ChunkTypeImageGenerationCallPartialImage]{
			ItemId: "ig_completed_only", PartialImageBase64: "preview",
		}},
		completed,
	}}

	var published []*responses.ResponseChunk
	response, err := executeGeneratedStream(t.Context(), []agents.ModelCallMiddleware{
		agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{Store: store}),
	}, provider, &published)
	require.NoError(t, err)
	require.Equal(t, 1, store.puts)
	require.Len(t, published, 1)
	result := published[0].OfResponseCompleted.Response.Output[0].OfImageGenerationCall.Result
	require.True(t, attachments.IsFileID(result))
	require.Equal(t, result, response.Output[0].OfImageGenerationCall.Result)
	require.Equal(t, raw, completed.OfResponseCompleted.Response.Output[0].OfImageGenerationCall.Result)
}

func TestAttachmentMiddlewareStreamUploadFailureIsTerminalAndPublishesNoBytes(t *testing.T) {
	raw := base64.StdEncoding.EncodeToString(pngBytes(t))
	provider := &generatedStreamProvider{chunks: []*responses.ResponseChunk{
		{OfImageGenerationCallPartialImage: &responses.ChunkImageGenerationCall[constants.ChunkTypeImageGenerationCallPartialImage]{PartialImageBase64: "preview"}},
		{OfOutputItemDone: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{Item: responses.ChunkOutputItemData{
			Type: "image_generation_call", Id: "ig_denied", OutputFormat: utils.Ptr("png"), Result: &raw,
		}}},
		{OfResponseCompleted: &responses.ChunkResponse[constants.ChunkTypeResponseCompleted]{}},
	}}
	var published []*responses.ResponseChunk
	response, err := executeGeneratedStream(t.Context(), []agents.ModelCallMiddleware{
		agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{Store: rejectedUpload{}}),
		agentmiddleware.NewRetry(agentmiddleware.RetryConfig{MaxAttempts: 3, InitialBackoff: time.Nanosecond}),
	}, provider, &published)
	require.Nil(t, response)
	require.ErrorIs(t, err, attachments.ErrDenied)
	require.True(t, agents.IsModelCallAborted(err))
	require.True(t, agents.IsModelStreamTransformError(err))
	require.Equal(t, 1, provider.calls, "middleware storage failures are not provider-retryable")
	require.Empty(t, published, "neither previews nor the offending completed image may be published")
}

type cancellationUpload struct {
	attachments.UploadStore
	started chan struct{}
}

func (s *cancellationUpload) Put(ctx context.Context, _ string, _ attachments.Upload) (attachments.Ref, error) {
	close(s.started)
	<-ctx.Done()
	return attachments.Ref{}, ctx.Err()
}

type drainingProvider struct {
	llm.Provider
	raw  string
	done chan struct{}
}

func (p *drainingProvider) NewStreamingResponses(ctx context.Context, _ *responses.Request) (chan *responses.ResponseChunk, error) {
	stream := make(chan *responses.ResponseChunk)
	go func() {
		defer close(p.done)
		defer close(stream)
		stream <- &responses.ResponseChunk{OfOutputItemDone: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{Item: responses.ChunkOutputItemData{
			Type: "image_generation_call", Id: "ig_cancel", OutputFormat: utils.Ptr("png"), Result: &p.raw,
		}}}
		select {
		case stream <- &responses.ResponseChunk{OfResponseCompleted: &responses.ChunkResponse[constants.ChunkTypeResponseCompleted]{}}:
		case <-ctx.Done():
		}
	}()
	return stream, nil
}

func TestAttachmentMiddlewareStreamUploadCancellationCancelsAndDrainsProvider(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	store := &cancellationUpload{started: make(chan struct{})}
	provider := &drainingProvider{raw: base64.StdEncoding.EncodeToString(pngBytes(t)), done: make(chan struct{})}
	var published []*responses.ResponseChunk
	result := make(chan error, 1)
	go func() {
		_, err := executeGeneratedStream(ctx, []agents.ModelCallMiddleware{
			agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{Store: store}),
		}, provider, &published)
		result <- err
	}()
	select {
	case <-store.started:
	case <-time.After(time.Second):
		t.Fatal("upload did not start")
	}
	cancel()
	select {
	case err := <-result:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("model call did not return after cancellation")
	}
	select {
	case <-provider.done:
	case <-time.After(time.Second):
		t.Fatal("provider sender was not released")
	}
	require.Empty(t, published)
}

func TestAttachmentMiddlewareHydratesGeneratedImageHistory(t *testing.T) {
	store := middlewareStore(t)
	ref, err := store.Put(t.Context(), "test", attachments.Upload{
		Filename: "generated.png", MediaType: "image/png", Content: bytes.NewReader(pngBytes(t)),
	})
	require.NoError(t, err)
	fileID := attachments.FileID(ref)
	request := &responses.Request{Input: responses.InputUnion{OfInputMessageList: []responses.InputMessageUnion{{
		OfImageGenerationCall: &responses.ImageGenerationCallMessage{
			ID: "ig_history", Status: "completed", Background: "opaque", OutputFormat: "png",
			Quality: "medium", Size: "1024x1024", Result: fileID,
		},
	}}}}

	sent, err := prepared(t, agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{InlineAttachments: true, Store: store}), t.Context(), request)
	require.NoError(t, err)
	require.NotSame(t, request, sent)
	require.Equal(t, base64.StdEncoding.EncodeToString(pngBytes(t)), sent.Input.OfInputMessageList[0].OfImageGenerationCall.Result)
	require.Equal(t, "ig_history", sent.Input.OfInputMessageList[0].OfImageGenerationCall.ID)
	require.Equal(t, "opaque", sent.Input.OfInputMessageList[0].OfImageGenerationCall.Background)
	require.Equal(t, "medium", sent.Input.OfInputMessageList[0].OfImageGenerationCall.Quality)
	require.Equal(t, "1024x1024", sent.Input.OfInputMessageList[0].OfImageGenerationCall.Size)
	require.Equal(t, fileID, request.Input.OfInputMessageList[0].OfImageGenerationCall.Result, "history stays referenced")

	_, err = agentmiddleware.PrepareAttachments(t.Context(), "another-namespace", request, attachments.NewResolver(store, attachments.Config{}), 0)
	require.ErrorIs(t, err, attachments.ErrNotFound)
	_, err = agentmiddleware.PrepareAttachments(t.Context(), "test", request, attachments.NewResolver(store, attachments.Config{}), 8)
	require.ErrorIs(t, err, attachments.ErrTooLarge)
}

func TestAttachmentMiddlewareGeneratedImageFailuresStayInsideModelCall(t *testing.T) {
	raw := base64.StdEncoding.EncodeToString(pngBytes(t))
	for _, tc := range []struct {
		name   string
		cfg    agentmiddleware.AttachmentMiddlewareConfig
		result string
		format string
		want   error
	}{
		{"missing store", agentmiddleware.AttachmentMiddlewareConfig{}, raw, "png", attachments.ErrUnresolved},
		{"malformed base64", agentmiddleware.AttachmentMiddlewareConfig{Store: middlewareStore(t)}, "!!!!", "png", attachments.ErrInvalid},
		{"not an image", agentmiddleware.AttachmentMiddlewareConfig{Store: middlewareStore(t)}, base64.StdEncoding.EncodeToString([]byte("plain text")), "", attachments.ErrInvalid},
		{"format mismatch", agentmiddleware.AttachmentMiddlewareConfig{Store: middlewareStore(t)}, raw, "jpeg", attachments.ErrInvalid},
		{"too large", agentmiddleware.AttachmentMiddlewareConfig{Store: middlewareStore(t), MaxFileBytes: 1}, raw, "png", attachments.ErrTooLarge},
		{"storage denied", agentmiddleware.AttachmentMiddlewareConfig{Store: rejectedUpload{}}, raw, "png", attachments.ErrDenied},
		{"malformed owned ref", agentmiddleware.AttachmentMiddlewareConfig{}, "attachment://../secret", "png", attachments.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := &responses.Response{Output: []responses.OutputMessageUnion{{
				OfImageGenerationCall: &responses.ImageGenerationCallMessage{ID: "ig_bad", OutputFormat: tc.format, Result: tc.result},
			}}}
			out, err := agents.ExecuteModelCallWithMiddleware(t.Context(), []agents.ModelCallMiddleware{
				agentmiddleware.NewAttachmentMiddleware(tc.cfg),
			}, &agents.ModelCall{Namespace: "test"}, &responses.Request{}, func(context.Context, *agents.ModelCall, *responses.Request) (*responses.Response, error) {
				return response, nil
			})
			require.Nil(t, out)
			require.ErrorIs(t, err, tc.want)
			require.True(t, agents.IsModelCallAborted(err), "middleware failures must be terminal to durable runtimes")
			require.Equal(t, tc.result, response.Output[0].OfImageGenerationCall.Result)
		})
	}
}

// On its way to the model, every reference in the request becomes the bytes
// behind it — on a copy, so the request the loop keeps still holds references.
func TestAttachmentMiddleware_ResolvesReferencesForTheModel(t *testing.T) {
	store := middlewareStore(t)
	image, file := uploadedRefs(t, store)
	middleware := agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{InlineAttachments: true, Store: store})
	request := referencedRequest(image, file)
	before, err := json.Marshal(request)
	require.NoError(t, err)

	sent, err := prepared(t, middleware, t.Context(), request)
	require.NoError(t, err)
	require.NotSame(t, request, sent)

	wire, err := json.Marshal(sent)
	require.NoError(t, err)
	require.Contains(t, string(wire), "data:image/png;base64,")
	require.Contains(t, string(wire), "data:application/pdf;base64,")
	require.NotContains(t, string(wire), "attachment://")
	require.Equal(t, "report.pdf", *sent.Input.OfInputMessageList[1].OfFunctionCallOutput.Output.OfList[0].OfInputFile.FileName,
		"the document keeps the name it was stored under")

	after, err := json.Marshal(request)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after), "the request handed in is left as it was")

	// Nothing to resolve: the provider is handed the request as it was, not a copy.
	plain := &responses.Request{Input: responses.InputUnion{OfInputMessageList: []responses.InputMessageUnion{responses.UserMessage("hi")}}}
	same, err := prepared(t, middleware, t.Context(), plain)
	require.NoError(t, err)
	require.Same(t, plain, same)
}

func TestAttachmentMiddlewarePreservesProviderFileIDs(t *testing.T) {
	// No store or resolver: provider IDs must work without consulting either.
	middleware := agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{})
	content := responses.InputContent{
		{OfInputImage: &responses.InputImageContent{FileID: utils.Ptr("file-provider-image")}},
		{OfInputFile: &responses.InputFileContent{FileID: utils.Ptr("file-provider-document")}},
	}
	request := &responses.Request{Input: responses.InputUnion{OfInputMessageList: []responses.InputMessageUnion{
		{OfEasyInput: &responses.EasyMessage{Role: constants.RoleUser, Content: responses.EasyInputContentUnion{OfInputMessageList: content}}},
	}}}
	sent, err := prepared(t, middleware, t.Context(), request)
	require.NoError(t, err)
	require.Same(t, request, sent)
	result := &agents.ToolCallResponse{FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{Output: responses.FunctionCallOutputContentUnion{OfList: content}}}
	out, err := externalized(middleware, t.Context(), result)
	require.NoError(t, err)
	require.Same(t, result, out)
}

// A resolver handed in is the one used, which is what lets several agents
// share one byte cache. With no store to build one over, it is the only way
// a reference could have been read.
func TestAttachmentMiddleware_UsesTheResolverItIsGiven(t *testing.T) {
	store := middlewareStore(t)
	image, file := uploadedRefs(t, store)
	middleware := agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{InlineAttachments: true, Resolver: attachments.NewResolver(store, attachments.Config{})})

	sent, err := prepared(t, middleware, t.Context(), referencedRequest(image, file))
	require.NoError(t, err)
	require.Contains(t, *sent.Input.OfInputMessageList[0].OfInputMessage.Content[1].OfInputImage.ImageURL, "data:image/png;base64,")
}

// A reference that cannot be read fails the call: the provider must not be
// sent one it cannot read either.
func TestAttachmentMiddleware_FailsClosedOnAReferenceItCannotRead(t *testing.T) {
	store := middlewareStore(t)
	image, file := uploadedRefs(t, store)

	// Neither a resolver nor a store to build one over.
	_, err := prepared(t, agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{InlineAttachments: true}), t.Context(), referencedRequest(image, file))
	require.ErrorIs(t, err, attachments.ErrUnresolved)

	// Nothing to read, no reference: the request passes through.
	plain := &responses.Request{Input: responses.InputUnion{OfInputMessageList: []responses.InputMessageUnion{responses.UserMessage("hi")}}}
	same, err := prepared(t, agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{InlineAttachments: true}), t.Context(), plain)
	require.NoError(t, err)
	require.Same(t, plain, same)

	// A reference nobody uploaded.
	_, err = prepared(t, agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{InlineAttachments: true, Store: store}), t.Context(), referencedRequest(attachments.Ref{ID: "missing"}, file))
	require.Error(t, err)

	// More than one request may carry.
	_, err = prepared(t, agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{InlineAttachments: true, Store: store, MaxInlineBytes: 16}), t.Context(), referencedRequest(image, file))
	require.ErrorIs(t, err, attachments.ErrTooLarge)
}

func TestPrepareCopiesEveryContentContainerAndTranslatesInline(t *testing.T) {
	ctx := context.Background()
	store, err := attachments.NewFileStore(t.TempDir(), attachments.FileStoreConfig{})
	require.NoError(t, err)
	defer store.Close()
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a6xkAAAAASUVORK5CYII=")
	require.NoError(t, err)
	ref, err := store.Put(ctx, "a", attachments.Upload{Filename: "a.png", MediaType: "image/png", Content: bytes.NewReader(png)})
	require.NoError(t, err)
	pdf, err := store.Put(ctx, "a", attachments.Upload{Filename: "a.pdf", MediaType: "application/pdf", Content: bytes.NewBufferString("%PDF-1.7\n")})
	require.NoError(t, err)
	content := responses.InputContent{{OfInputImage: &responses.InputImageContent{FileID: utils.Ptr(attachments.FileID(ref))}}, {OfInputFile: &responses.InputFileContent{FileID: utils.Ptr(attachments.FileID(pdf))}}}
	in := &responses.Request{Input: responses.InputUnion{OfInputMessageList: []responses.InputMessageUnion{
		{OfEasyInput: &responses.EasyMessage{Content: responses.EasyInputContentUnion{OfInputMessageList: content}}},
		{OfInputMessage: &responses.InputMessage{Content: content}},
		{OfFunctionCallOutput: &responses.FunctionCallOutputMessage{Output: responses.FunctionCallOutputContentUnion{OfList: content}}},
	}}}
	original, err := sonic.Marshal(in)
	require.NoError(t, err)
	resolver := attachments.NewResolver(store, attachments.Config{})
	out, err := agentmiddleware.PrepareAttachments(ctx, "a", in, resolver, 0)
	require.NoError(t, err)
	after, err := sonic.Marshal(in)
	require.NoError(t, err)
	require.Equal(t, string(original), string(after))
	prepared, err := sonic.Marshal(out)
	require.NoError(t, err)
	require.NotContains(t, string(prepared), "attachment://")
	require.Contains(t, string(prepared), "data:image/png;base64,")
	require.Contains(t, string(prepared), "data:application/pdf;base64,")
	// Serializing/reloading conversation input retains owned file references.
	var loaded responses.Request
	require.NoError(t, sonic.Unmarshal(original, &loaded))
	require.Equal(t, attachments.FileID(ref), *loaded.Input.OfInputMessageList[0].OfEasyInput.Content.OfInputMessageList[0].OfInputImage.FileID)
	a, err := sonic.Marshal(anthropic.NativeRequestToRequest(out))
	require.NoError(t, err)
	require.Contains(t, string(a), `"type":"base64"`)
	require.NotContains(t, string(a), "attachment://")
	g, err := sonic.Marshal(gemini.ResponsesInputToGeminiResponsesInput(out))
	require.NoError(t, err)
	require.Contains(t, string(g), "inlineData")
	require.NotContains(t, string(g), "attachment://")
	b, err := sonic.Marshal(bedrock.NativeRequestToConverseRequest(out))
	require.NoError(t, err)
	require.Contains(t, string(b), `"bytes"`)
	require.NotContains(t, string(b), "attachment://")
	_, err = agentmiddleware.PrepareAttachments(ctx, "a", in, nil, 0)
	require.ErrorIs(t, err, attachments.ErrUnresolved)
	_, err = agentmiddleware.PrepareAttachments(ctx, "a", in, resolver, 10)
	require.ErrorIs(t, err, attachments.ErrTooLarge)
	url := "https://example.invalid/image"
	content[0].OfInputImage.ImageURL = &url
	_, err = agentmiddleware.PrepareAttachments(ctx, "a", in, resolver, 0)
	require.ErrorIs(t, err, attachments.ErrInvalid)
}

func TestPreparationPreservesEmptyAndTextOnlyInputs(t *testing.T) {
	for _, in := range []*responses.Request{
		{Input: responses.InputUnion{OfInputMessageList: []responses.InputMessageUnion{}}},
		{Input: responses.InputUnion{OfInputMessageList: []responses.InputMessageUnion{{OfInputMessage: &responses.InputMessage{Content: responses.InputContent{}}}}}},
		{Input: responses.InputUnion{OfInputMessageList: []responses.InputMessageUnion{responses.UserMessage("hello")}}},
	} {
		before, err := sonic.Marshal(in)
		require.NoError(t, err)
		out, err := agentmiddleware.PrepareAttachments(context.Background(), "a", in, nil, 0)
		require.NoError(t, err)
		after, err := sonic.Marshal(out)
		require.NoError(t, err)
		require.Equal(t, string(before), string(after))
	}
}

func TestPreparationCountsRepeatedInlineContent(t *testing.T) {
	data := "data:image/png;base64,AAAA"
	content := responses.InputContent{{OfInputImage: &responses.InputImageContent{ImageURL: &data}}, {OfInputImage: &responses.InputImageContent{ImageURL: &data}}}
	in := &responses.Request{Input: responses.InputUnion{OfInputMessageList: []responses.InputMessageUnion{{OfInputMessage: &responses.InputMessage{Content: content}}}}}
	_, err := agentmiddleware.PrepareAttachments(context.Background(), "a", in, nil, int64(len(data)))
	require.ErrorIs(t, err, attachments.ErrTooLarge)
}

func TestAttachmentMiddlewareDefaultsToTextReferencesWithoutStorageReads(t *testing.T) {
	// An embedded nil store panics on any storage access, including through the
	// supplied resolver. Even nonexistent or oversized files need no reads.
	unreadable := struct{ attachments.UploadStore }{}
	for _, cfg := range []agentmiddleware.AttachmentMiddlewareConfig{
		{},
		{Store: unreadable, Resolver: attachments.NewResolver(unreadable, attachments.Config{}), MaxFileBytes: 1, MaxInlineBytes: 1},
	} {
		id := "attachment://missing-file"
		content := responses.InputContent{
			{OfInputText: &responses.InputTextContent{Text: "inspect with a tool"}},
			{OfInputImage: &responses.InputImageContent{FileID: &id}},
			{OfInputFile: &responses.InputFileContent{FileID: &id, FileName: utils.Ptr("large.pdf")}},
		}
		request := &responses.Request{Input: responses.InputUnion{OfInputMessageList: []responses.InputMessageUnion{
			{OfEasyInput: &responses.EasyMessage{Role: constants.RoleUser, Content: responses.EasyInputContentUnion{OfInputMessageList: content}}},
			{OfInputMessage: &responses.InputMessage{Role: constants.RoleUser, Content: content}},
			{OfFunctionCallOutput: &responses.FunctionCallOutputMessage{CallID: "call", Output: responses.FunctionCallOutputContentUnion{OfList: content}}},
			{OfImageGenerationCall: &responses.ImageGenerationCallMessage{ID: "generated", Result: id}},
		}}}
		before, err := json.Marshal(request)
		require.NoError(t, err)
		sent, err := prepared(t, agentmiddleware.NewAttachmentMiddleware(cfg), t.Context(), request)
		require.NoError(t, err)
		require.NotSame(t, request, sent)
		for _, parts := range []responses.InputContent{
			sent.Input.OfInputMessageList[0].OfEasyInput.Content.OfInputMessageList,
			sent.Input.OfInputMessageList[1].OfInputMessage.Content,
			sent.Input.OfInputMessageList[2].OfFunctionCallOutput.Output.OfList,
		} {
			require.Equal(t, "inspect with a tool", parts[0].OfInputText.Text)
			for _, part := range parts[1:] {
				require.Nil(t, part.OfInputImage)
				require.Nil(t, part.OfInputFile)
				require.Contains(t, part.OfInputText.Text, id)
				require.Contains(t, part.OfInputText.Text, "File contents are not included")
			}
		}
		require.Equal(t, "call", sent.Input.OfInputMessageList[2].OfFunctionCallOutput.CallID)
		generated := sent.Input.OfInputMessageList[3]
		require.Nil(t, generated.OfImageGenerationCall)
		require.Equal(t, constants.RoleAssistant, generated.OfEasyInput.Role)
		require.Contains(t, *generated.OfEasyInput.Content.OfString, id)
		after, err := json.Marshal(request)
		require.NoError(t, err)
		require.JSONEq(t, string(before), string(after))
		for _, wire := range []interface{}{
			sent, anthropic.NativeRequestToRequest(sent), gemini.ResponsesInputToGeminiResponsesInput(sent), bedrock.NativeRequestToConverseRequest(sent),
		} {
			encoded, err := json.Marshal(wire)
			require.NoError(t, err)
			require.Contains(t, string(encoded), id)
			require.NotContains(t, string(encoded), "base64")
		}
	}
}

func TestAttachmentMiddlewareTextReferencesRejectInvalidSources(t *testing.T) {
	id := "attachment://file"
	for _, part := range []responses.InputContentUnion{
		{OfInputImage: &responses.InputImageContent{FileID: utils.Ptr("attachment://../secret")}},
		{OfInputImage: &responses.InputImageContent{FileID: &id, ImageURL: utils.Ptr("data:image/png;base64,AAAA")}},
		{OfInputFile: &responses.InputFileContent{FileID: &id, FileData: utils.Ptr("AAAA")}},
		{OfInputFile: &responses.InputFileContent{FileID: &id, FileURL: utils.Ptr("https://example.com/file")}},
	} {
		req := &responses.Request{Input: responses.InputUnion{OfInputMessageList: []responses.InputMessageUnion{{OfInputMessage: &responses.InputMessage{Content: responses.InputContent{part}}}}}}
		_, err := prepared(t, agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{}), t.Context(), req)
		require.ErrorIs(t, err, attachments.ErrInvalid)
	}
}
