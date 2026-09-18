package agui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/attachments"
	"github.com/stretchr/testify/require"
)

type namespaceRuntime func(context.Context, *agents.AgentInput) (*agents.AgentOutput, error)

func (f namespaceRuntime) Run(ctx context.Context, _ *agents.Agent, in *agents.AgentInput) (*agents.AgentOutput, error) {
	return f(ctx, in)
}

func TestNamespaceResolverDefaultsAndSingleAgent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		resolve NamespaceResolver
		want    string
	}{
		{name: "missing", want: "default"},
		{name: "empty", resolve: func(*http.Request) (string, error) { return "", nil }, want: "default"},
		{name: "blank", resolve: func(*http.Request) (string, error) { return " \t", nil }, want: "default"},
		{name: "custom", resolve: func(*http.Request) (string, error) { return "tenant", nil }, want: "tenant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			received := make(chan string, 1)
			a := agents.NewAgent(&agents.AgentOptions{Name: "Helper", Runtime: namespaceRuntime(func(_ context.Context, in *agents.AgentInput) (*agents.AgentOutput, error) {
				received <- in.Namespace
				return &agents.AgentOutput{}, nil
			})})
			h := AgentHandler(a, WithNamespaceResolver(tc.resolve))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("POST", "/custom", strings.NewReader(`{"threadId":"thread","messages":[{"id":"m","role":"user","content":"hello"}]}`)))
			require.Equal(t, 200, w.Code, w.Body.String())
			require.Equal(t, tc.want, <-received)
		})
	}
}

func TestNamespaceResolverErrorsRejectEveryEndpoint(t *testing.T) {
	store, err := attachments.NewFileStore(t.TempDir(), attachments.FileStoreConfig{})
	require.NoError(t, err)
	defer store.Close()
	calls := 0
	resolve := WithNamespaceResolver(func(r *http.Request) (string, error) {
		calls++
		if strings.Contains(r.URL.Path, "/Helper/") {
			require.Equal(t, "Helper", r.PathValue("agent"))
		}
		return "", errors.New("private identity error")
	})
	a := agents.NewAgent(&agents.AgentOptions{Name: "Helper"})
	h := NewHandler(registry{"Helper": a}, resolve, WithAttachmentStore(store))
	for _, route := range []struct{ method, path string }{
		{"GET", "/agents"}, {"POST", "/agents/Helper/run"}, {"POST", "/agents/Helper/stop"},
		{"GET", "/agents/Helper/threads"}, {"GET", "/agents/Helper/threads/t/messages"},
		{"GET", "/agents/Helper/threads/t/stream"}, {"GET", "/agents/Helper/runs"},
		{"POST", "/attachments/"}, {"GET", "/attachments/file"},
	} {
		w := httptest.NewRecorder()
		before := calls
		h.ServeHTTP(w, httptest.NewRequest(route.method, route.path, nil))
		require.Equal(t, before+1, calls)
		require.Equal(t, 403, w.Code, route.path)
		require.NotContains(t, w.Body.String(), "private identity error")
		require.NotContains(t, w.Header().Get("Content-Type"), "text/event-stream")
	}
	w := httptest.NewRecorder()
	AgentHandler(a, resolve).ServeHTTP(w, httptest.NewRequest("POST", "/custom", nil))
	require.Equal(t, 403, w.Code)
}

func TestNamespaceResolverConcurrentRunsHistoryAndRejoin(t *testing.T) {
	const tenants = 12
	steps := make([]scriptedStep, tenants)
	for i := range steps {
		steps[i] = scriptedStep{response: assistantTextResponse("done")}
	}
	a := agents.NewAgent(&agents.AgentOptions{Name: "Helper"}).WithLLM(&scriptedLLM{steps: steps})
	var calls atomic.Int64
	h := NewHandler(registry{"Helper": a}, WithNamespaceResolver(func(r *http.Request) (string, error) {
		calls.Add(1)
		return r.Header.Get("X-Test-Tenant"), nil
	}))
	request := func(method, path, tenant, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-Test-Tenant", tenant)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	cursors := make(map[string]string)
	for i := 0; i < tenants; i++ {
		ns := fmt.Sprintf("tenant-%d", i)
		w := request("GET", "/agents/Helper/runs?wait=0", ns, "")
		var feed FeedResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &feed))
		cursors[ns] = feed.Cursor
	}
	var wg sync.WaitGroup
	for i := 0; i < tenants; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ns := fmt.Sprintf("tenant-%d", i)
			w := request("POST", "/agents/Helper/run", ns, fmt.Sprintf(`{"threadId":"shared","messages":[{"id":"m","role":"user","content":"%s"}]}`, ns))
			require.Equal(t, 200, w.Code, w.Body.String())
			require.Equal(t, agents.StreamIDForThread(ns, "shared"), w.Header().Get("X-Stream-Id"))
		}(i)
	}
	wg.Wait()
	for i := 0; i < tenants; i++ {
		ns := fmt.Sprintf("tenant-%d", i)
		w := request("GET", "/agents/Helper/threads/shared/messages", ns, "")
		require.Equal(t, 200, w.Code)
		var transcript struct {
			Messages []Message `json:"messages"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &transcript))
		require.NotEmpty(t, transcript.Messages)
		require.Equal(t, ns, transcript.Messages[0].Content)
		w = request("GET", "/agents/Helper/threads", ns, "")
		require.Equal(t, 200, w.Code)
		require.Contains(t, w.Body.String(), "shared")
		w = request("GET", "/agents/Helper/threads/shared/stream", ns, "")
		require.Equal(t, 200, w.Code)
		require.Equal(t, agents.StreamIDForThread(ns, "shared"), w.Header().Get("X-Stream-Id"))
		w = request("GET", "/agents/Helper/runs?wait=0&cursor="+url.QueryEscape(cursors[ns]), ns, "")
		var feed FeedResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &feed))
		require.NotEmpty(t, feed.Events)
		for _, event := range feed.Events {
			require.Equal(t, ns, event.Namespace)
		}
	}
	require.Equal(t, int64(tenants*6), calls.Load(), "resolve once per request")
	w := request("GET", "/agents/Helper/threads", "", "")
	require.NotContains(t, w.Body.String(), "shared")
}

func TestNamespaceResolverAttachmentUploadDownloadAndRun(t *testing.T) {
	store, err := attachments.NewFileStore(t.TempDir(), attachments.FileStoreConfig{})
	require.NoError(t, err)
	defer store.Close()
	a := agents.NewAgent(&agents.AgentOptions{Name: "Helper", Runtime: namespaceRuntime(func(_ context.Context, in *agents.AgentInput) (*agents.AgentOutput, error) {
		require.Equal(t, "alice", in.Namespace)
		return &agents.AgentOutput{}, nil
	})})
	calls := 0
	h := http.StripPrefix("/api/agui", NewHandler(registry{"Helper": a}, WithAttachmentStore(store), WithNamespaceResolver(func(r *http.Request) (string, error) { calls++; return r.Header.Get("X-Test-Tenant"), nil })))
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	file, err := form.CreateFormFile("file", "note.txt")
	require.NoError(t, err)
	_, err = file.Write([]byte("private note"))
	require.NoError(t, err)
	require.NoError(t, form.WriteField("session_id", "alice"))
	require.NoError(t, form.Close())
	req := httptest.NewRequest("POST", "/api/agui/attachments/", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("X-Test-Tenant", "alice")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, 201, w.Code, w.Body.String())
	var uploaded attachments.HTTPFile
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &uploaded))
	for _, ns := range []string{"alice", "bob"} {
		req = httptest.NewRequest("GET", uploaded.URL, nil)
		req.Header.Set("X-Test-Tenant", ns)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if ns == "alice" {
			require.Equal(t, 200, w.Code)
			require.Equal(t, "private note", w.Body.String())
		} else {
			require.NotEqual(t, 200, w.Code)
		}
		input := RunAgentInput{ThreadID: ns, Messages: []Message{{ID: "m", Role: RoleUser, ContentParts: []ContentPart{{Type: "document", Source: &ContentSource{Type: "url", Value: uploaded.FileID}}}}}}
		data, err := json.Marshal(input)
		require.NoError(t, err)
		req = httptest.NewRequest("POST", "/api/agui/agents/Helper/run", bytes.NewReader(data))
		req.Header.Set("X-Test-Tenant", ns)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if ns == "alice" {
			require.Equal(t, 200, w.Code, w.Body.String())
		} else {
			require.Equal(t, 400, w.Code, w.Body.String())
		}
	}
	require.Equal(t, 5, calls)
}

func (namespaceRuntime) StreamBroker() agents.StreamBroker { return nil }

func (namespaceRuntime) RegisterAgent(*agents.AgentOptions) error { return nil }
