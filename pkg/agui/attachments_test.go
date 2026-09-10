package agui

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"strings"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	agentmiddleware "github.com/hastekit/agent-sdk-go/pkg/agents/middleware"
	"github.com/hastekit/agent-sdk-go/pkg/attachments"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

func TestMultipartMessageReferencesAndHistory(t *testing.T) {
	store, err := attachments.NewFileStore(t.TempDir(), attachments.FileStoreConfig{})
	require.NoError(t, err)
	defer store.Close()
	ctx := context.Background()
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a6xkAAAAASUVORK5CYII=")
	require.NoError(t, err)
	for _, tc := range []struct {
		kind, name, mime string
		data             []byte
	}{{"image", "photo.png", "image/png", png}, {"document", "report.pdf", "application/pdf", []byte("%PDF-1.7\nexample\n%%EOF")}} {
		t.Run(tc.kind, func(t *testing.T) {
			ref, err := store.Put(ctx, "alice", attachments.Upload{Filename: tc.name, MediaType: tc.mime, Content: bytes.NewReader(tc.data)})
			require.NoError(t, err)
			msg := Message{ID: "user-1", Role: RoleUser, ContentParts: []ContentPart{{Type: tc.kind, Source: &ContentSource{Type: "url", Value: attachments.URL(ref), MimeType: "untrusted"}}}}
			wire, err := json.Marshal(msg)
			require.NoError(t, err)
			var decoded Message
			require.NoError(t, json.Unmarshal(wire, &decoded))
			require.NoError(t, validateMessageAttachments(ctx, "alice", []Message{decoded}, store))
			require.Equal(t, tc.mime, decoded.ContentParts[0].Source.MimeType)
			require.Error(t, validateMessageAttachments(ctx, "bob", []Message{decoded}, store))
			in := RunAgentInput{ThreadID: "thread", Messages: []Message{decoded}}
			sdkMsgs := in.ToSDKMessages()
			require.Len(t, sdkMsgs, 1)
			req := &responses.Request{Input: responses.InputUnion{OfInputMessageList: sdkMsgs}}
			hydrated, err := agentmiddleware.PrepareAttachments(ctx, "alice", req, attachments.NewResolver(store, attachments.Config{}), 0)
			require.NoError(t, err)
			c := hydrated.Input.OfInputMessageList[0].OfInputMessage.Content[0]
			if tc.kind == "image" {
				require.NotNil(t, c.OfInputImage)
				require.True(t, strings.HasPrefix(*c.OfInputImage.ImageURL, "data:image/png;base64,"))
			} else {
				require.NotNil(t, c.OfInputFile)
				require.Equal(t, "report.pdf", *c.OfInputFile.FileName)
				require.True(t, strings.HasPrefix(*c.OfInputFile.FileData, "data:application/pdf;base64,"))
			}
			persisted, err := json.Marshal(req)
			require.NoError(t, err)
			require.Contains(t, string(persisted), "attachment://")
			require.NotContains(t, string(persisted), "base64")
			require.NotContains(t, string(persisted), "/attachments/")
			rows := []history.ConversationMessage{{Messages: []history.Message{{Messages: sdkMsgs}}}}
			restored := HistoryToMessages(rows)
			require.Len(t, restored, 1)
			require.Equal(t, tc.kind, restored[0].ContentParts[0].Type)
			require.Equal(t, attachments.URL(ref), restored[0].ContentParts[0].Source.Value)
			// Persistence can decode user input into either SDK union arm.
			rows[0].Messages[0].Messages = []responses.InputMessageUnion{{OfEasyInput: &responses.EasyMessage{Role: constants.RoleUser, Content: responses.EasyInputContentUnion{OfInputMessageList: sdkMsgs[0].OfInputMessage.Content}}}}
			require.Len(t, HistoryToMessages(rows), 1)
		})
	}
}

func TestMultipartRejectsInlineExternalAndUnsupported(t *testing.T) {
	for _, source := range []ContentSource{{Type: "data", Value: "AAAA"}, {Type: "url", Value: "https://example.com/file.png"}} {
		msg := Message{Role: RoleUser, ContentParts: []ContentPart{{Type: "image", Source: &source}}}
		require.Error(t, validateMessageAttachments(context.Background(), "alice", []Message{msg}, nil))
	}
	var msg Message
	require.NoError(t, json.Unmarshal([]byte(`{"id":"1","role":"user","content":"hello"}`), &msg))
	require.Equal(t, "hello", msg.Content)
	encoded, err := json.Marshal(msg)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"content":"hello"`)
}

func TestMultipartInputEvent(t *testing.T) {
	ref := &attachments.Ref{ID: "owned", Version: "v1"}
	events := NewTranslator("thread", "run").Translate(&responses.ResponseChunk{OfInputMessage: &responses.ChunkInputMessage[constants.ChunkTypeInputMessage]{MessageID: "msg_1", Role: "user", ContentParts: responses.InputContent{{OfInputImage: &responses.InputImageContent{FileID: utils.Ptr(attachments.FileID(*ref))}}}}})
	require.Len(t, events, 1)
	event, ok := events[0].(*CustomEvent)
	require.True(t, ok)
	require.Equal(t, "input_message", event.Name)
	msg, ok := event.Value.(Message)
	require.True(t, ok)
	require.Equal(t, attachments.URL(*ref), msg.ContentParts[0].Source.Value)
}
