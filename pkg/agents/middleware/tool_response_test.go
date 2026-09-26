package middleware_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/attachments"
	agentmiddleware "github.com/hastekit/agent-sdk-go/pkg/agents/middleware"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/require"
)

// responseFile mirrors the public JSON contract presented to the model.
type responseFile struct {
	FileID       string `json:"file_id"`
	MountPath    string `json:"mount_path"`
	Filename     string `json:"original_filename"`
	MediaType    string `json:"mime_type"`
	Size         int64  `json:"size_bytes"`
	Summary      string `json:"summary"`
	Instructions string `json:"instructions"`
}

// responseStore exposes a model-visible mount distinct from its private host directory.
func responseStore(t *testing.T) *attachments.FileStore {
	t.Helper()
	store, err := attachments.NewFileStore(t.TempDir(), attachments.FileStoreConfig{MountPath: "/uploads"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

// readResponseFile validates the replacement and reads the complete result under its execution scope.
func readResponseFile(t *testing.T, store attachments.Store, result *agents.ToolCallResponse) (responseFile, []byte) {
	t.Helper()
	require.NotNil(t, result.Output.OfString)
	require.Nil(t, result.Output.OfList)
	var info responseFile
	require.NoError(t, json.Unmarshal([]byte(*result.Output.OfString), &info))
	ref, err := attachments.RefFromFileID(info.FileID)
	require.NoError(t, err)
	descriptor, err := store.Lookup(t.Context(), "test", "thread", ref)
	require.NoError(t, err)
	reader, err := store.Open(t.Context(), descriptor)
	require.NoError(t, err)
	defer reader.Close()
	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	return info, data
}

// Text and JSON retain their exact bytes, while control fields and the original result remain untouched.
func TestToolResponseOffloadRoundTrip(t *testing.T) {
	for _, jsonOutput := range []bool{false, true} {
		name, content, mimeType := "text", strings.Repeat("hello 世界\n", 1000), "text/plain"
		if jsonOutput {
			name, content, mimeType = "json", `{"records":"`+strings.Repeat("abcdef", 1000)+`"}`, "application/json"
		}
		t.Run(name, func(t *testing.T) {
			store := responseStore(t)
			limiter := agentmiddleware.NewToolResponseMiddleware(agentmiddleware.ToolResponseMiddlewareConfig{
				Store: store, MaxResponseBytes: 1024, MaxSummaryBytes: 256,
			})
			original := &agents.ToolCallResponse{
				FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
					ID: "result", CallID: "call", Output: responses.FunctionCallOutputContentUnion{OfString: &content},
				},
				StateUpdates: map[string]string{"status": "ready"},
				Interrupts:   []responses.Interrupt{{}}, TaskID: "task", TaskPayload: json.RawMessage(`{"cursor":"next"}`),
			}

			// The replacement fits the budget and points at the original, immutable bytes.
			result, err := externalized(limiter, t.Context(), original)
			require.NoError(t, err)
			info, data := readResponseFile(t, store, result)
			require.Equal(t, content, string(data))
			require.Equal(t, int64(len(data)), info.Size)
			require.Equal(t, mimeType, info.MediaType)
			require.True(t, strings.HasPrefix(info.MountPath, "/uploads/tool-response"))
			require.NotEmpty(t, info.Summary)
			require.LessOrEqual(t, len(info.Summary), 256)
			require.LessOrEqual(t, len(*result.Output.OfString), 1024)

			// Only the output payload changes; execution control information stays intact.
			require.NotSame(t, original, result)
			require.NotSame(t, original.FunctionCallOutputMessage, result.FunctionCallOutputMessage)
			require.Equal(t, original.ID, result.ID)
			require.Equal(t, original.CallID, result.CallID)
			require.Equal(t, original.StateUpdates, result.StateUpdates)
			require.Equal(t, original.Interrupts, result.Interrupts)
			require.Equal(t, original.TaskID, result.TaskID)
			require.Equal(t, original.TaskPayload, result.TaskPayload)
			require.Equal(t, content, *original.Output.OfString)

			// An unrelated session cannot read the saved response.
			ref, err := attachments.RefFromFileID(info.FileID)
			require.NoError(t, err)
			_, err = store.Lookup(t.Context(), "test", "other-session", ref)
			require.ErrorIs(t, err, attachments.ErrDenied)
		})
	}
}

// Filenames distinguish calls and remain stable across retries of the same call.
func TestToolResponseCallIDFilenames(t *testing.T) {
	store := responseStore(t)
	limiter := agentmiddleware.NewToolResponseMiddleware(agentmiddleware.ToolResponseMiddlewareConfig{Store: store, MaxResponseBytes: 1024})
	for _, callID := range []string{"call_first", "call_second", "call_first"} {
		content := strings.Repeat("large result", 1000)
		original := &agents.ToolCallResponse{FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			Output: responses.FunctionCallOutputContentUnion{OfString: &content},
		}}
		call := &agents.ToolCall{
			Namespace: "test", SessionID: "thread",
			FunctionCallMessage: &responses.FunctionCallMessage{ID: "item", CallID: callID},
		}
		result, err := limiter.WrapToolCall(func(context.Context, *agents.BaseTool, *agents.ToolCall) (*agents.ToolCallResponse, error) {
			return original, nil
		})(t.Context(), nil, call)
		require.NoError(t, err)
		info, data := readResponseFile(t, store, result)

		// Use the call ID from the invocation, not the item ID or the result's missing ID.
		require.Equal(t, content, string(data))
		require.Equal(t, "tool-response-"+callID+".txt", info.Filename)
	}
}

// Without storage, oversized output becomes a bounded preview with honest retrieval guidance.
func TestToolResponseWithoutStore(t *testing.T) {
	limiter := agentmiddleware.NewToolResponseMiddleware(agentmiddleware.ToolResponseMiddlewareConfig{
		MaxResponseBytes: 512, MaxSummaryBytes: 128,
	})
	for _, call := range []*agents.ToolCall{nil, {}} {
		content := strings.Repeat("large response 世界 ", 1000)
		original := &agents.ToolCallResponse{
			FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
				ID: "result", CallID: "call", Output: responses.FunctionCallOutputContentUnion{OfString: &content},
			},
			StateUpdates: map[string]string{"status": "ready"}, TaskID: "task",
		}
		result, err := limiter.WrapToolCall(func(context.Context, *agents.BaseTool, *agents.ToolCall) (*agents.ToolCallResponse, error) {
			return original, nil
		})(t.Context(), nil, call)
		require.NoError(t, err)

		// No execution scope is needed when no file is written, and no file reference is invented.
		var fields map[string]any
		require.NoError(t, json.Unmarshal([]byte(*result.Output.OfString), &fields))
		for _, key := range []string{"file_id", "mount_path", "original_filename"} {
			require.NotContains(t, fields, key)
		}
		require.EqualValues(t, len(content), fields["size_bytes"])
		require.Contains(t, fields["instructions"], "was not saved")
		require.Contains(t, fields["instructions"], "another tool")
		require.Contains(t, fields["summary"], "large response")
		require.LessOrEqual(t, len(fields["summary"].(string)), 128)
		require.LessOrEqual(t, len(*result.Output.OfString), 512)

		// Control fields and the original response survive the replacement unchanged.
		require.Equal(t, original.ID, result.ID)
		require.Equal(t, original.CallID, result.CallID)
		require.Equal(t, original.StateUpdates, result.StateUpdates)
		require.Equal(t, original.TaskID, result.TaskID)
		require.Equal(t, content, *original.Output.OfString)
	}
}

// Multipart output remains recoverable, and previews never repeat inline media bytes.
func TestToolResponseOffloadsMultipart(t *testing.T) {
	store := responseStore(t)
	inline := "data:image/png;base64," + strings.Repeat("AAAA", 1000)
	content := responses.InputContent{
		{OfInputText: &responses.InputTextContent{Text: "Image analysis"}},
		{OfInputImage: &responses.InputImageContent{ImageURL: &inline}},
	}
	original := &agents.ToolCallResponse{FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
		Output: responses.FunctionCallOutputContentUnion{OfList: content},
	}}
	limiter := agentmiddleware.NewToolResponseMiddleware(agentmiddleware.ToolResponseMiddlewareConfig{Store: store, MaxResponseBytes: 1024})
	result, err := externalized(limiter, t.Context(), original)
	require.NoError(t, err)
	info, data := readResponseFile(t, store, result)

	// The stored array preserves all content, while its summary describes only text and structure.
	expected, err := json.Marshal(content)
	require.NoError(t, err)
	require.Equal(t, expected, data)
	require.Equal(t, "application/json", info.MediaType)
	require.Contains(t, info.Summary, "1 images")
	require.Contains(t, info.Summary, "Image analysis")
	require.NotContains(t, *result.Output.OfString, "AAAA")
	require.Equal(t, content, original.Output.OfList)
}

// Boundary-sized, absent and failing outputs do not touch attachment storage.
func TestToolResponsePassThrough(t *testing.T) {
	limiter := agentmiddleware.NewToolResponseMiddleware(agentmiddleware.ToolResponseMiddlewareConfig{MaxResponseBytes: 1024})
	for _, size := range []int{0, 100, 1024} {
		original := &agents.ToolCallResponse{FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr(strings.Repeat("x", size))},
		}}
		result, err := externalized(limiter, t.Context(), original)
		require.NoError(t, err)
		require.Same(t, original, result)
	}

	// Nil results and results carrying only control metadata are valid pass-through values.
	for _, original := range []*agents.ToolCallResponse{nil, {TaskID: "task"}, {FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{}}} {
		result, err := externalized(limiter, t.Context(), original)
		require.NoError(t, err)
		require.Equal(t, original, result)
	}

	// A tool error remains the same error, even if it accompanies a large result.
	wantErr := errors.New("tool failed")
	original := &agents.ToolCallResponse{FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
		Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr(strings.Repeat("x", 4096))},
	}}
	result, err := limiter.WrapToolCall(func(context.Context, *agents.BaseTool, *agents.ToolCall) (*agents.ToolCallResponse, error) {
		return original, wantErr
	})(t.Context(), nil, nil)
	require.Same(t, original, result)
	require.ErrorIs(t, err, wantErr)
}

// Escaped characters consume their JSON size, while multibyte previews end on valid boundaries.
func TestToolResponseReplacementBudget(t *testing.T) {
	for _, repeated := range []string{"世界😀", "\"\n\t\\<>&", "\xff\xfe"} {
		store := responseStore(t)
		content := strings.Repeat(repeated, 2000)
		original := &agents.ToolCallResponse{FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			Output: responses.FunctionCallOutputContentUnion{OfString: &content},
		}}
		limiter := agentmiddleware.NewToolResponseMiddleware(agentmiddleware.ToolResponseMiddlewareConfig{
			Store: store, MaxResponseBytes: 600, MaxSummaryBytes: 4096,
		})
		result, err := externalized(limiter, t.Context(), original)
		require.NoError(t, err)
		info, data := readResponseFile(t, store, result)

		// Full bytes survive even when a safe preview needs truncation or UTF-8 normalization.
		require.Equal(t, content, string(data))
		require.True(t, utf8.ValidString(info.Summary))
		require.NotEmpty(t, info.Summary)
		require.LessOrEqual(t, len(*result.Output.OfString), 600)
	}
}

// Failed metadata lookup cannot leave the model with a misleading file reference.
type failedResponseLookup struct{ attachments.UploadStore }

func (failedResponseLookup) Lookup(context.Context, string, string, attachments.Ref) (attachments.Descriptor, error) {
	return attachments.Descriptor{}, attachments.ErrDenied
}

// Storage and configuration failures abort the middleware instead of returning unbounded content.
func TestToolResponseOffloadFailures(t *testing.T) {
	store := responseStore(t)
	call := &agents.ToolCall{Namespace: "test", SessionID: "thread", FunctionCallMessage: &responses.FunctionCallMessage{CallID: "call"}}
	tests := []struct {
		name  string
		store attachments.UploadStore
		call  *agents.ToolCall
		limit int
		want  error
	}{
		{"missing scope", store, &agents.ToolCall{}, 1024, attachments.ErrDenied},
		{"missing call ID", store, &agents.ToolCall{Namespace: "test", SessionID: "thread"}, 1024, attachments.ErrInvalid},
		{"upload denied", rejectedUpload{}, call, 1024, attachments.ErrDenied},
		{"lookup denied", failedResponseLookup{store}, call, 1024, attachments.ErrDenied},
		{"metadata cannot fit", store, call, 1, attachments.ErrTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limiter := agentmiddleware.NewToolResponseMiddleware(agentmiddleware.ToolResponseMiddlewareConfig{Store: tt.store, MaxResponseBytes: tt.limit})
			original := &agents.ToolCallResponse{FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
				Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr(strings.Repeat("x", 4096))},
			}}
			result, err := agents.ExecuteToolWithMiddleware(t.Context(), []agents.ToolCallMiddleware{limiter}, agents.ExecutableToolCall{ToolCall: tt.call},
				func(context.Context, *agents.ToolCall) (*agents.ToolCallResponse, error) { return original, nil })

			// The executor recognizes storage errors as middleware aborts, not model-visible tool failures.
			require.Nil(t, result)
			require.ErrorIs(t, err, tt.want)
			require.True(t, agents.IsToolCallAborted(err))
			require.Len(t, *original.Output.OfString, 4096)
		})
	}
}

// A returned file summary stays compact even when attachment hydration is enabled for model calls.
func TestToolResponseDoesNotRehydrate(t *testing.T) {
	store := responseStore(t)
	limiter := agentmiddleware.NewToolResponseMiddleware(agentmiddleware.ToolResponseMiddlewareConfig{Store: store, MaxResponseBytes: 1024})
	original := &agents.ToolCallResponse{FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
		Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr(strings.Repeat("large output", 1000))},
	}}
	result, err := externalized(limiter, t.Context(), original)
	require.NoError(t, err)
	request := &responses.Request{Input: responses.InputUnion{OfInputMessageList: []responses.InputMessageUnion{{OfFunctionCallOutput: result.FunctionCallOutputMessage}}}}

	// The attachment middleware sees ordinary text metadata rather than an inlineable content part.
	prepared, err := prepared(t, agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{Store: store, InlineAttachments: true}), t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, *result.Output.OfString, *prepared.Input.OfInputMessageList[0].OfFunctionCallOutput.Output.OfString)
}

// Attachment externalization runs before the outer response limiter stores a multipart result.
func TestToolResponseWithAttachmentMiddleware(t *testing.T) {
	store := responseStore(t)
	original := attachmentResult()
	original.Output.OfList[0].OfInputText.Text = strings.Repeat("Analysis of attached files. ", 1000)
	call := &agents.ToolCall{Namespace: "test", SessionID: "thread", FunctionCallMessage: &responses.FunctionCallMessage{ID: "result", CallID: "call"}}
	middlewares := []agents.ToolCallMiddleware{
		agentmiddleware.NewToolResponseMiddleware(agentmiddleware.ToolResponseMiddlewareConfig{Store: store, MaxResponseBytes: 1024}),
		agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{Store: store}),
	}
	result, err := agents.ExecuteToolWithMiddleware(t.Context(), middlewares, agents.ExecutableToolCall{ToolCall: call},
		func(context.Context, *agents.ToolCall) (*agents.ToolCallResponse, error) { return original, nil })
	require.NoError(t, err)
	info, data := readResponseFile(t, store, result)

	// The saved JSON keeps references to separately uploaded media, with no inline binary payloads.
	require.Contains(t, string(data), "attachment://")
	require.NotContains(t, string(data), "data:image")
	require.Contains(t, info.Summary, "1 images, 1 files")
	require.LessOrEqual(t, len(*result.Output.OfString), 1024)
	require.NotNil(t, original.Output.OfList[1].OfInputImage.ImageURL)
}

// Cancellation and invalid unions fail before attempting storage.
func TestToolResponseRejectsInvalidOrCancelledOutput(t *testing.T) {
	store := &countingUploads{UploadStore: responseStore(t)}
	limiter := agentmiddleware.NewToolResponseMiddleware(agentmiddleware.ToolResponseMiddlewareConfig{Store: store, MaxResponseBytes: 1024})
	original := &agents.ToolCallResponse{FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
		Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr(strings.Repeat("x", 4096))},
	}}

	// An already-cancelled execution must not leave an uploaded artifact behind.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err := externalized(limiter, ctx, original)
	require.Nil(t, result)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, store.puts)

	// A malformed union cannot silently lose one of its content branches.
	original.Output.OfList = responses.InputContent{{OfInputText: &responses.InputTextContent{Text: "other branch"}}}
	result, err = externalized(limiter, t.Context(), original)
	require.Nil(t, result)
	require.ErrorIs(t, err, attachments.ErrInvalid)
	require.Zero(t, store.puts)
}
