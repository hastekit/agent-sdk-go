package middleware_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	agentmiddleware "github.com/hastekit/agent-sdk-go/pkg/agents/middleware"
	"github.com/hastekit/agent-sdk-go/pkg/attachments"
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

// On its way to the model, every reference in the request becomes the bytes
// behind it — on a copy, so the request the loop keeps still holds references.
func TestAttachmentMiddleware_ResolvesReferencesForTheModel(t *testing.T) {
	store := middlewareStore(t)
	image, file := uploadedRefs(t, store)
	middleware := agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{Store: store})
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
	middleware := agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{Resolver: attachments.NewResolver(store, attachments.Config{})})

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
	_, err := prepared(t, agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{}), t.Context(), referencedRequest(image, file))
	require.ErrorIs(t, err, attachments.ErrUnresolved)

	// Nothing to read, no reference: the request passes through.
	plain := &responses.Request{Input: responses.InputUnion{OfInputMessageList: []responses.InputMessageUnion{responses.UserMessage("hi")}}}
	same, err := prepared(t, agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{}), t.Context(), plain)
	require.NoError(t, err)
	require.Same(t, plain, same)

	// A reference nobody uploaded.
	_, err = prepared(t, agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{Store: store}), t.Context(), referencedRequest(attachments.Ref{ID: "missing"}, file))
	require.Error(t, err)

	// More than one request may carry.
	_, err = prepared(t, agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{Store: store, MaxInlineBytes: 16}), t.Context(), referencedRequest(image, file))
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
