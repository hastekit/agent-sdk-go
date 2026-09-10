package sdk

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	agentmiddleware "github.com/hastekit/agent-sdk-go/pkg/agents/middleware"
	"github.com/hastekit/agent-sdk-go/pkg/attachments"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type countingAttachmentStore struct {
	attachments.Store
	opens atomic.Int32
}

func (s *countingAttachmentStore) Open(ctx context.Context, d attachments.Descriptor) (io.ReadCloser, error) {
	s.opens.Add(1)
	return s.Store.Open(ctx, d)
}

// attachmentFixture is a store with one image in it under the "tenant" scope,
// wrapped so the test can count how often storage is actually read.
func attachmentFixture(t *testing.T) (context.Context, *countingAttachmentStore, attachments.Ref) {
	t.Helper()
	fs, err := attachments.NewFileStore(t.TempDir(), attachments.FileStoreConfig{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = fs.Close() })
	ctx := context.Background()
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a6xkAAAAASUVORK5CYII=")
	require.NoError(t, err)
	ref, err := fs.Put(ctx, "tenant", attachments.Upload{Filename: "photo.png", MediaType: "image/png", Content: bytes.NewReader(png)})
	require.NoError(t, err)
	return ctx, &countingAttachmentStore{Store: fs}, ref
}

// imageTurn is a user turn as the agent receives it: a reference, no bytes.
func imageTurn(id string, ref attachments.Ref) history.Message {
	return history.Message{ID: id, SenderID: "user", Messages: []responses.InputMessageUnion{{
		OfInputMessage: &responses.InputMessage{Role: constants.RoleUser, Content: responses.InputContent{
			{OfInputText: &responses.InputTextContent{Text: "What is in this picture?"}},
			{OfInputImage: &responses.InputImageContent{FileID: utils.Ptr(attachments.FileID(ref))}},
		}},
	}}}
}

// completedStream answers a streaming Responses request with an empty,
// completed response, which ends the agent's turn.
func completedStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"output\":[],\"usage\":{}}}\n\n")
}

func runTurn(t *testing.T, agent *Agent, ctx context.Context, thread string, msg history.Message) error {
	t.Helper()
	handle, err := agent.Execute(ctx, &agents.AgentInput{Namespace: "tenant", ThreadID: thread, Message: msg})
	require.NoError(t, err)
	_, err = handle.Result()
	return err
}

// The middleware resolves the request before the client is called, so everything on
// the client's chain — here a retry — sees bytes and never a reference; the
// resolver's cache means storage is read once across a retry and a second
// turn; and history keeps the reference the turn arrived with.
func TestAttachmentMiddlewareResolvesBeforeTheClientAcrossTurnsAndRetries(t *testing.T) {
	ctx, store, ref := attachmentFixture(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		assert.NotContains(t, string(body), "attachment://")
		assert.Contains(t, string(body), "data:image/png;base64,")
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
			_, _ = io.WriteString(w, `{"error":{"message":"retry"}}`)
			return
		}
		completedStream(w)
	}))
	defer server.Close()
	configs := testConfigs()
	configs[0].BaseURL = server.URL
	client := NewLLMClient(configs, WithMiddleware(middleware.NewRetry(middleware.RetryConfig{MaxAttempts: 2, InitialBackoff: 1})))

	hist := NewFileHistory(t.TempDir())
	agent := NewAgent(&AgentConfig{
		Name:        "attachment-retry-test",
		LLM:         client.Model("OpenAI/vision"),
		History:     hist,
		Middlewares: []agents.Middleware{agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{Resolver: attachments.NewResolver(store, attachments.Config{})})},
	})

	require.NoError(t, runTurn(t, agent, ctx, "thread", imageTurn("first", ref)))
	require.NoError(t, runTurn(t, agent, ctx, "thread", history.Message{ID: "second", SenderID: "user", Messages: []responses.InputMessageUnion{responses.UserMessage("And again?")}}))

	// Turn one cost two provider calls (the 503 and its retry), turn two one
	// more — and the image, still in history as a reference, went out as bytes
	// every time.
	require.EqualValues(t, 3, calls.Load())
	require.EqualValues(t, 1, store.opens.Load(), "one storage read, shared by the retry and the next turn")

	rows, err := hist.LoadTranscript(ctx, "tenant", "thread")
	require.NoError(t, err)
	raw, err := json.Marshal(rows)
	require.NoError(t, err)
	require.Contains(t, string(raw), "attachment://")
	require.NotContains(t, string(raw), "base64")
}

// A fallback to another provider re-sends the request the middleware prepared, so
// the second provider sees bytes too, from the same single storage read.
func TestAttachmentMiddlewareResolvesBeforeProviderFallback(t *testing.T) {
	ctx, store, ref := attachmentFixture(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		assert.NotContains(t, string(body), "attachment://")
		assert.Contains(t, string(body), "data:image/png;base64,")
		calls.Add(1)
		if strings.HasPrefix(r.URL.Path, "/first/") {
			w.WriteHeader(503)
			_, _ = io.WriteString(w, `{"error":{"message":"unavailable"}}`)
			return
		}
		completedStream(w)
	}))
	defer server.Close()
	client := NewLLMClient([]ProviderConfig{
		{ProviderName: ProviderOpenAI, BaseURL: server.URL + "/first", ApiKeys: []*APIKeyConfig{{APIKey: "first"}}},
		{ProviderName: ProviderOllama, BaseURL: server.URL + "/fallback", ApiKeys: []*APIKeyConfig{{APIKey: "second"}}},
	}, WithMiddleware(middleware.NewFallbackModels("Ollama/vision")))

	agent := NewAgent(&AgentConfig{
		Name:        "attachment-fallback-test",
		LLM:         client.Model("OpenAI/vision"),
		History:     NewFileHistory(t.TempDir()),
		Middlewares: []agents.Middleware{agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{Resolver: attachments.NewResolver(store, attachments.Config{})})},
	})

	require.NoError(t, runTurn(t, agent, ctx, "thread", imageTurn("first", ref)))
	require.EqualValues(t, 2, calls.Load())
	require.EqualValues(t, 1, store.opens.Load())
}

// A reference the middleware will not send — here one over the request's inline
// budget — fails the turn before any provider is contacted.
func TestAttachmentMiddlewareLimitsFailBeforeTheProviderIsCalled(t *testing.T) {
	ctx, store, ref := attachmentFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the provider must not be called")
	}))
	defer server.Close()
	configs := testConfigs()
	configs[0].BaseURL = server.URL

	agent := NewAgent(&AgentConfig{
		Name:        "attachment-limit-test",
		LLM:         NewLLMClient(configs).Model("OpenAI/vision"),
		History:     NewFileHistory(t.TempDir()),
		Middlewares: []agents.Middleware{agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{Resolver: attachments.NewResolver(store, attachments.Config{}), MaxInlineBytes: 1})},
	})

	err := runTurn(t, agent, ctx, "thread", imageTurn("first", ref))
	require.ErrorIs(t, err, attachments.ErrTooLarge)
}

// Without the middleware the client sends the request exactly as written: a
// reference stays a reference, and the SDK does not reach for the store.
func TestWithoutTheAttachmentMiddlewareTheClientSendsTheRequestAsWritten(t *testing.T) {
	ctx, store, ref := attachmentFixture(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		assert.Contains(t, string(body), "attachment://")
		assert.NotContains(t, string(body), "base64")
		calls.Add(1)
		completedStream(w)
	}))
	defer server.Close()
	configs := testConfigs()
	configs[0].BaseURL = server.URL

	agent := NewAgent(&AgentConfig{
		Name:    "attachment-unmiddlewareed-test",
		LLM:     NewLLMClient(configs).Model("OpenAI/vision"),
		History: NewFileHistory(t.TempDir()),
	})

	require.NoError(t, runTurn(t, agent, ctx, "thread", imageTurn("first", ref)))
	require.EqualValues(t, 1, calls.Load())
	require.EqualValues(t, 0, store.opens.Load())
}
