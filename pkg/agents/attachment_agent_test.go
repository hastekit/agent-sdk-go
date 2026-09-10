package agents_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	agentmiddleware "github.com/hastekit/agent-sdk-go/pkg/agents/middleware"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/attachments"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/require"
)

// These tests run the attachment middleware inside a real agent loop. The middleware's own
// unit tests live with it, in pkg/agents/middleware.

// attachmentResult is a tool result carrying an image and a document inline.
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

func middlewareStore(t *testing.T) *attachments.FileStore {
	t.Helper()
	store, err := attachments.NewFileStore(t.TempDir(), attachments.FileStoreConfig{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// uploadedImage puts attachmentResult's image in the store and returns its
// reference, which is the shape a user's turn arrives in.
func uploadedImage(t *testing.T, store attachments.UploadStore) attachments.Ref {
	t.Helper()
	uri := *attachmentResult().Output.OfList[1].OfInputImage.ImageURL
	png, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(uri, "data:image/png;base64,"))
	require.NoError(t, err)
	ref, err := store.Put(t.Context(), "ns", attachments.Upload{Filename: "pixel.png", MediaType: "image/png", Content: bytes.NewReader(png)})
	require.NoError(t, err)
	return ref
}

type mediaResultTool struct{ *agents.BaseTool }

func (m *mediaResultTool) Execute(_ context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
	r := attachmentResult()
	r.ID, r.CallID = call.ID, call.CallID
	return r, nil
}

// What a tool returns inline reaches history as references.
func TestAttachmentMiddlewareAgentHistoryKeepsReferences(t *testing.T) {
	store := middlewareStore(t)
	persistence := history.NewInMemoryConversationPersistence()
	manager := history.NewConversationManager(persistence)
	model := &scriptedLLM{script: []*responses.Response{toolCallResponse("call", "media", "{}"), textResponse("done")}}
	tool := &mediaResultTool{BaseTool: &agents.BaseTool{ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{Name: "media"}}}}
	agent := agents.NewAgent(&agents.AgentOptions{Name: "media-agent", Tools: []agents.Tool{tool}, History: manager, Middlewares: []agents.Middleware{agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{Store: store})}}).WithLLM(model)
	out := runAgent(t, agent, &agents.AgentInput{Namespace: "ns", ThreadID: "thread", Message: userMessage("make files")})
	requireStatus(t, out, agentstate.RunStatusCompleted)
	rows, err := manager.LoadTranscript(t.Context(), "ns", "thread")
	require.NoError(t, err)
	raw, err := json.Marshal(rows)
	require.NoError(t, err)
	require.Contains(t, string(raw), "attachment://")
	require.NotContains(t, string(raw), "base64")
	require.Equal(t, 2, model.callCount())
}

// A background task's result goes through the middleware before the local runner
// delivers it, so it too reaches history as references.
func TestAttachmentMiddlewareExternalizesBackgroundResultsBeforeLocalDelivery(t *testing.T) {
	store := middlewareStore(t)
	tool := newBackgroundTool("media", "task-media")
	tool.result = agents.BackgroundResult{Output: attachmentResult().FunctionCallOutputMessage}
	model := &scriptedLLM{script: []*responses.Response{toolCallResponse("call", "media", "{}"), textResponse("started"), textResponse("finished")}}
	manager := history.NewConversationManager(history.NewInMemoryConversationPersistence())
	agent := agents.NewAgent(&agents.AgentOptions{Name: "background-media", Tools: []agents.Tool{tool}, History: manager, Middlewares: []agents.Middleware{agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{Store: store})}}).WithLLM(model)
	out := runAgent(t, agent, &agents.AgentInput{Namespace: "ns", ThreadID: "background-media", Message: userMessage("make files")})
	requireStatus(t, out, agentstate.RunStatusCompleted)
	close(tool.release)
	agent.WaitForBackgroundTasks()
	rows, err := manager.LoadTranscript(t.Context(), "ns", "background-media")
	require.NoError(t, err)
	raw, err := json.Marshal(rows)
	require.NoError(t, err)
	require.Contains(t, string(raw), "attachment://")
	require.NotContains(t, string(raw), "base64")
	require.Equal(t, 3, model.callCount())
}

// Inside a run the two halves meet on one middleware: what a tool returned as bytes
// is stored as a reference, and what history holds as a reference reaches the
// model as bytes — with nothing durable ever holding the bytes.
func TestAttachmentMiddleware_AgentSendsBytesAndKeepsReferences(t *testing.T) {
	store := middlewareStore(t)
	image := uploadedImage(t, store)
	manager := history.NewConversationManager(history.NewInMemoryConversationPersistence())
	model := &scriptedLLM{script: []*responses.Response{toolCallResponse("call", "media", "{}"), textResponse("done")}}
	tool := &mediaResultTool{BaseTool: &agents.BaseTool{ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{Name: "media"}}}}
	agent := agents.NewAgent(&agents.AgentOptions{
		Name:        "media-agent",
		Tools:       []agents.Tool{tool},
		History:     manager,
		Middlewares: []agents.Middleware{agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{Store: store})},
	}).WithLLM(model)

	// The user's turn arrives as history shapes it: a reference, no bytes.
	turn := messages.New("user", []responses.InputMessageUnion{{OfInputMessage: &responses.InputMessage{Role: constants.RoleUser, Content: responses.InputContent{
		{OfInputText: &responses.InputTextContent{Text: "describe this, then make files"}},
		{OfInputImage: &responses.InputImageContent{FileID: utils.Ptr(attachments.FileID(image))}},
	}}}})
	out := runAgent(t, agent, &agents.AgentInput{Namespace: "ns", ThreadID: "thread", Message: turn})
	requireStatus(t, out, agentstate.RunStatusCompleted)
	require.Equal(t, 2, model.callCount())

	// The model was sent bytes on both calls: for the user's image, and on the
	// second call for the files the tool returned — which by then had been
	// stored as references.
	for i := range 2 {
		wire, err := json.Marshal(model.request(i))
		require.NoError(t, err)
		require.Contains(t, string(wire), "data:image/png;base64,", "call %d", i)
		require.NotContains(t, string(wire), "attachment://", "call %d", i)
	}
	second, err := json.Marshal(model.request(1))
	require.NoError(t, err)
	require.Contains(t, string(second), "data:application/pdf;base64,")

	// History holds references and nothing else.
	rows, err := manager.LoadTranscript(t.Context(), "ns", "thread")
	require.NoError(t, err)
	stored, err := json.Marshal(rows)
	require.NoError(t, err)
	require.Contains(t, string(stored), "attachment://")
	require.NotContains(t, string(stored), "base64")
}

// A task started by a handoff target goes through that target's middlewares, not the
// owner's: only the specialist is configured to externalize its media, and the
// root that owns the run has no middlewares at all.
func TestAttachmentMiddlewareAppliesToAHandoffTargetsBackgroundTask(t *testing.T) {
	store := middlewareStore(t)
	broker := streambroker.NewMemoryStreamBroker()
	manager := history.NewConversationManager(history.NewInMemoryConversationPersistence())

	tool := newBackgroundTool("media", "task-media")
	tool.result = agents.BackgroundResult{Output: attachmentResult().FunctionCallOutputMessage}
	specialistLLM := &scriptedLLM{script: []*responses.Response{toolCallResponse("call_1", "media", "{}"), textResponse("started")}}
	specialist := agents.NewAgent(&agents.AgentOptions{
		Name:         "specialist",
		Tools:        []agents.Tool{tool},
		StreamBroker: broker,
		Middlewares:  []agents.Middleware{agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{Store: store})},
	}).WithLLM(specialistLLM)

	rootLLM := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("h1", "transfer_to_agent", `{"agent_name":"specialist"}`),
		textResponse("finished"),
	}}
	root := agents.NewAgent(&agents.AgentOptions{
		Name:         "root",
		History:      manager,
		StreamBroker: broker,
		Handoffs:     []*agents.Handoff{agents.NewHandoff("specialist", "makes files", specialist)},
	}).WithLLM(rootLLM)

	out := runAgent(t, root, &agents.AgentInput{
		Namespace: "ns", ThreadID: "thread-handoff-media",
		StreamID: agents.StreamIDForThread("ns", "thread-handoff-media"),
		Message:  userMessage("make files"),
	})
	requireStatus(t, out, agentstate.RunStatusCompleted)

	close(tool.release)
	root.WaitForBackgroundTasks()

	rows, err := manager.LoadTranscript(t.Context(), "ns", "thread-handoff-media")
	require.NoError(t, err)
	raw, err := json.Marshal(rows)
	require.NoError(t, err)
	require.Contains(t, string(raw), "attachment://")
	require.NotContains(t, string(raw), "base64")
}
