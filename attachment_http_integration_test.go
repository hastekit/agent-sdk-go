package sdk

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	agentmiddleware "github.com/hastekit/agent-sdk-go/pkg/agents/middleware"
	"github.com/hastekit/agent-sdk-go/pkg/agui"
	"github.com/hastekit/agent-sdk-go/pkg/agui/web"
	"github.com/hastekit/agent-sdk-go/pkg/attachments"
	"github.com/stretchr/testify/require"
)

func TestChatAttachmentHTTPToProviderAndHistory(t *testing.T) {
	store, err := attachments.NewFileStore(t.TempDir(), attachments.FileStoreConfig{})
	require.NoError(t, err)
	defer store.Close()
	var mu sync.Mutex
	var requests []string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"output\":[],\"usage\":{}}}\n\n")
	}))
	defer provider.Close()
	configs := testConfigs()
	configs[0].BaseURL = provider.URL
	cacheStore := &countingAttachmentStore{Store: store}
	// The middleware on the agent is what resolves the browser's references for the
	// model; the client is given nothing and sends what it is handed.
	NewAgent(&AgentConfig{
		Name:        "attachment-http-test",
		LLM:         NewLLMClient(configs).Model("OpenAI/gpt-4o"),
		History:     NewFileHistory(t.TempDir()),
		Middlewares: []agents.Middleware{agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{Resolver: attachments.NewResolver(cacheStore, attachments.Config{})})},
	})
	// Uploads, the runs that read them, and the transcript all live under the
	// handler's namespace; nothing has to be stamped by hand.
	server := httptest.NewServer(web.Handler(&AgentRegistry{}, agui.WithAttachmentStore(store)))
	defer server.Close()
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a6xkAAAAASUVORK5CYII=")
	require.NoError(t, err)
	var parts []agui.ContentPart
	for _, tc := range []struct {
		kind, name string
		data       []byte
	}{{"image", "image.png", png}, {"document", "report.pdf", []byte("%PDF-1.7\nexample\n%%EOF")}} {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		part, err := form.CreateFormFile("file", tc.name)
		require.NoError(t, err)
		_, err = part.Write(tc.data)
		require.NoError(t, err)
		require.NoError(t, form.Close())
		res, err := http.Post(server.URL+"/attachments/", form.FormDataContentType(), &body)
		require.NoError(t, err)
		var file attachments.HTTPFile
		require.NoError(t, json.NewDecoder(res.Body).Decode(&file))
		res.Body.Close()
		require.Equal(t, 201, res.StatusCode)
		parts = append(parts, agui.ContentPart{Type: tc.kind, Source: &agui.ContentSource{Type: "url", Value: file.URL}})
	}
	for i, msg := range []agui.Message{{ID: "first", Role: agui.RoleUser, ContentParts: parts}, {ID: "second", Role: agui.RoleUser, Content: "Compare the two attachments again."}} {
		b, err := json.Marshal(agui.RunAgentInput{ThreadID: "attachments", Messages: []agui.Message{msg}})
		require.NoError(t, err)
		res, err := http.Post(server.URL+"/api/agui/agents/attachment-http-test/run", "application/json", bytes.NewReader(b))
		require.NoError(t, err)
		stream, err := io.ReadAll(res.Body)
		res.Body.Close()
		require.NoError(t, err)
		require.Equal(t, 200, res.StatusCode, string(stream))
		require.NotContains(t, string(stream), "RUN_ERROR")
		if i == 0 {
			require.Contains(t, string(stream), "input_message")
			require.Contains(t, string(stream), "/attachments/")
			require.NotContains(t, string(stream), "base64")
		}
	}
	mu.Lock()
	captured := append([]string(nil), requests...)
	mu.Unlock()
	require.Len(t, captured, 2)
	for _, body := range captured {
		require.Contains(t, body, "data:image/png;base64,")
		require.Contains(t, body, "data:application/pdf;base64,")
		require.Contains(t, body, `"filename":"report.pdf"`)
		require.NotContains(t, body, "attachment://")
		require.NotContains(t, body, "/attachments/")
	}
	require.EqualValues(t, 2, cacheStore.opens.Load(), "one storage read per file across both turns")
	res, err := http.Get(server.URL + "/api/agui/agents/attachment-http-test/threads/attachments/messages")
	require.NoError(t, err)
	b, err := io.ReadAll(res.Body)
	res.Body.Close()
	require.NoError(t, err)
	require.Equal(t, 200, res.StatusCode, string(b))
	require.NotContains(t, string(b), "base64")
	require.Contains(t, string(b), "/attachments/")
	var restored struct {
		Messages []agui.Message `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(b, &restored))
	require.GreaterOrEqual(t, len(restored.Messages), 2)
	require.Len(t, restored.Messages[0].ContentParts, 2)
	listing, err := http.Get(server.URL + "/api/agui/agents/attachment-http-test/threads")
	require.NoError(t, err)
	listed, err := io.ReadAll(listing.Body)
	listing.Body.Close()
	require.NoError(t, err)
	require.Equal(t, 200, listing.StatusCode, string(listed))
	require.Contains(t, string(listed), `"thread_id":"attachments"`)
	// Actual durable transcript holds owned refs, not transport representations.
	agent, _ := (&AgentRegistry{}).Agent("attachment-http-test")
	rows, err := agent.History().LoadTranscript(context.Background(), "default", "attachments")
	require.NoError(t, err)
	raw, err := json.Marshal(rows)
	require.NoError(t, err)
	require.Contains(t, string(raw), "attachment://")
	require.False(t, strings.Contains(string(raw), "base64"))
	require.NotContains(t, string(raw), "/attachments/")
}
