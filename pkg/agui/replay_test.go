package agui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

func replayIDs(body string) []string {
	var ids []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "id: ") {
			ids = append(ids, strings.TrimPrefix(line, "id: "))
		}
	}
	return ids
}

func TestResumeEveryEventFromInitialStreamAndCompletedReplay(t *testing.T) {
	a := agents.NewAgent(&agents.AgentOptions{Name: "Helper"}).WithLLM(&scriptedLLM{steps: []scriptedStep{{response: assistantTextResponse("hello")}}})
	h := NewHandler(registry{"Helper": a})
	initial := httptest.NewRecorder()
	h.ServeHTTP(initial, httptest.NewRequest("POST", "/agents/Helper/run", strings.NewReader(`{"threadId":"thread","runId":"client-run","messages":[{"id":"m","role":"user","content":"hello"}]}`)))
	require.Equal(t, 200, initial.Code)
	rows, err := a.History().LoadTranscript(context.Background(), "default", "thread")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "client-run", rows[0].RunID)
	require.Contains(t, initial.Body.String(), `"runId":"client-run"`)
	require.NotContains(t, initial.Body.String(), "external_run_id")
	ids := replayIDs(initial.Body.String())
	require.Greater(t, len(ids), 5)
	replay := httptest.NewRecorder()
	h.ServeHTTP(replay, httptest.NewRequest("GET", "/agents/Helper/threads/thread/stream", nil))
	require.Equal(t, 200, replay.Code)
	require.Equal(t, ids, replayIDs(replay.Body.String()), "POST and GET must assign identical stable IDs")
	require.Contains(t, replay.Body.String(), `"runId":"client-run"`)
	for i, id := range ids {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			req := httptest.NewRequest("GET", "/agents/Helper/threads/thread/stream", nil)
			req.Header.Set("Last-Event-ID", id)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			require.Equal(t, 200, w.Code, w.Body.String())
			require.Equal(t, ids[i+1:], append([]string{}, replayIDs(w.Body.String())...))
		})
	}
	req := httptest.NewRequest("GET", "/agents/Helper/threads/thread/stream?lastEventId="+url.QueryEscape(ids[2]), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, ids[3:], replayIDs(w.Body.String()))
}

func TestResumeRejectsInvalidWrongStreamAndStaleCursors(t *testing.T) {
	a := agents.NewAgent(&agents.AgentOptions{Name: "Helper"}).WithLLM(&scriptedLLM{steps: []scriptedStep{{response: assistantTextResponse("first")}, {response: assistantTextResponse("second")}}})
	h := NewHandler(registry{"Helper": a}, WithNamespaceResolver(func(r *http.Request) (string, error) { return r.Header.Get("X-Tenant"), nil }))
	start := func() string {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/agents/Helper/run", strings.NewReader(`{"threadId":"thread","messages":[{"id":"m","role":"user","content":"hello"}]}`)))
		require.Equal(t, 200, w.Code)
		return replayIDs(w.Body.String())[2]
	}
	id := start()
	for _, tc := range []struct {
		name, path, cursor, ns string
		status                 int
	}{
		{"invalid", "thread", "not-a-cursor", "", 400},
		{"wrong thread", "other", id, "", 400},
		{"wrong namespace", "thread", id, "other", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/agents/Helper/threads/"+tc.path+"/stream", nil)
			r.Header.Set("Last-Event-ID", tc.cursor)
			r.Header.Set("X-Tenant", tc.ns)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			require.Equal(t, tc.status, w.Code, w.Body.String())
			require.NotEqual(t, "text/event-stream", w.Header().Get("Content-Type"))
		})
	}
	start() // A fresh run on this thread replaces the retained transcript.
	r := httptest.NewRequest("GET", "/agents/Helper/threads/thread/stream", nil)
	r.Header.Set("Last-Event-ID", id)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	require.Equal(t, http.StatusGone, w.Code, w.Body.String())
}

func TestResumeAfterDisconnectedRunFinishes(t *testing.T) {
	gate := newGateTool("gate")
	a := agents.NewAgent(&agents.AgentOptions{Name: "Helper", Tools: []agents.Tool{gate}}).WithLLM(&scriptedLLM{steps: []scriptedStep{
		{response: toolCallResponse("call", "gate", "{}")}, {response: assistantTextResponse("done")},
	}})
	h := NewHandler(registry{"Helper": a})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest("POST", "/agents/Helper/run", strings.NewReader(`{"threadId":"thread","messages":[{"id":"m","role":"user","content":"hello"}]}`)).WithContext(ctx)
	initial := httptest.NewRecorder()
	returned := make(chan struct{})
	go func() { defer close(returned); h.ServeHTTP(initial, r) }()
	<-gate.entered
	cancel()
	<-returned
	ids := replayIDs(initial.Body.String())
	require.NotEmpty(t, ids)
	// The run is detached: reconnect after releasing the tool, and receive
	// exactly the remaining events, including its terminal event.
	close(gate.release)
	req := httptest.NewRequest("GET", "/agents/Helper/threads/thread/stream", nil)
	req.Header.Set("Last-Event-ID", ids[len(ids)-1])
	resumed := httptest.NewRecorder()
	h.ServeHTTP(resumed, req)
	require.Equal(t, 200, resumed.Code, resumed.Body.String())
	require.Contains(t, resumed.Body.String(), "RUN_FINISHED")
	complete := httptest.NewRecorder()
	h.ServeHTTP(complete, httptest.NewRequest("GET", "/agents/Helper/threads/thread/stream", nil))
	want := replayIDs(complete.Body.String())
	got := append(ids, replayIDs(resumed.Body.String())...)
	require.Equal(t, want, got)
}

func TestCursorDetectsChangedOrTrimmedEventPrefix(t *testing.T) {
	a := agents.NewAgent(&agents.AgentOptions{Name: "Helper"}).WithLLM(&scriptedLLM{steps: []scriptedStep{{response: assistantTextResponse("hello")}}})
	h := NewHandler(registry{"Helper": a})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/agents/Helper/run", strings.NewReader(`{"threadId":"thread","messages":[{"id":"m","role":"user","content":"hello"}]}`)))
	ids := replayIDs(w.Body.String())
	cursor, err := parseEventCursor(ids[len(ids)-1])
	require.NoError(t, err)
	chunks, err := a.StreamBroker().(agents.StreamReplayReader).Replay(context.Background(), cursor.Stream)
	require.NoError(t, err)
	require.NoError(t, validateReplay(chunks, "thread", cursor.Stream, cursor))
	require.ErrorIs(t, validateReplay(chunks[1:], "thread", cursor.Stream, cursor), errReplayUnavailable)
	// Preserve a structurally valid cursor but change its event-prefix digest.
	cursor.Hash = strings.Repeat("A", 43)
	require.ErrorIs(t, validateReplay(chunks, "thread", cursor.Stream, cursor), errReplayUnavailable)
}

func TestEncoderRejectsInjectedIDsAndOmitsIDForUnrecordedErrors(t *testing.T) {
	var out bytes.Buffer
	e := NewEncoder(&out)
	require.Error(t, e.EncodeWithID(&CustomEvent{Name: "test"}, "bad\nid: injected"))
	require.Empty(t, out.String())
	require.NoError(t, e.EncodeWithID(&RunErrorEvent{Message: "failed"}, ""))
	require.NotContains(t, out.String(), "id:")
}

// Ensure a cursor is opaque structured data rather than an unscoped counter.
func TestCursorRoundTrip(t *testing.T) {
	s := &streamTranslation{threadID: "thread", streamID: "stream", runID: "run"}
	id, err := s.eventID(&CustomEvent{Name: "test", Value: map[string]any{"text": "hello"}})
	require.NoError(t, err)
	cursor, err := parseEventCursor(id)
	require.NoError(t, err)
	require.Equal(t, id, cursor.String())
	require.Equal(t, "stream", cursor.Stream)
	_, err = parseEventCursor("42")
	require.Error(t, err)
}

// Model failures must survive persistence and broker replay with the same terminal event.
func TestFailedRunPersistsAndReplays(t *testing.T) {
	for _, backend := range []string{"memory", "file"} {
		t.Run(backend, func(t *testing.T) {
			var store history.ConversationPersistenceAdapter = history.NewInMemoryConversationPersistence()
			if backend == "file" {
				var err error
				store, err = history.NewFileConversationPersistence(t.TempDir())
				require.NoError(t, err)
			}
			model := &scriptedLLM{}
			a := agents.NewAgent(&agents.AgentOptions{Name: "Helper", History: history.NewConversationManager(store)}).WithLLM(model)
			h := NewHandler(registry{"Helper": a})
			start := func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest("POST", "/agents/Helper/run", strings.NewReader(`{"threadId":"failed-thread","messages":[{"id":"msg_user","role":"user","content":"hello"}]}`)))
				return w
			}
			live := start()
			require.Equal(t, 200, live.Code)
			require.Contains(t, live.Body.String(), `"type":"RUN_ERROR"`)
			require.NotContains(t, live.Body.String(), `"type":"RUN_FINISHED"`)
			replay := httptest.NewRecorder()
			h.ServeHTTP(replay, httptest.NewRequest("GET", "/agents/Helper/threads/failed-thread/stream", nil))
			require.Contains(t, replay.Body.String(), `"type":"RUN_ERROR"`)
			require.NotContains(t, replay.Body.String(), `"type":"RUN_FINISHED"`)
			require.Equal(t, replayIDs(live.Body.String()), replayIDs(replay.Body.String()))

			// Rejoin from immediately before the failure must deliver it with its stable cursor.
			ids := replayIDs(live.Body.String())
			require.GreaterOrEqual(t, len(ids), 2)
			request := httptest.NewRequest("GET", "/agents/Helper/threads/failed-thread/stream", nil)
			request.Header.Set("Last-Event-ID", ids[len(ids)-2])
			resumed := httptest.NewRecorder()
			h.ServeHTTP(resumed, request)
			require.Equal(t, ids[len(ids)-1:], replayIDs(resumed.Body.String()))
			require.Contains(t, resumed.Body.String(), `"type":"RUN_ERROR"`)

			// The history API restores errors even after broker retention expires.
			page := httptest.NewRecorder()
			h.ServeHTTP(page, httptest.NewRequest("GET", "/agents/Helper/threads/failed-thread/messages", nil))
			var body struct {
				Run *ThreadRunState `json:"run"`
			}
			require.NoError(t, json.Unmarshal(page.Body.Bytes(), &body))
			require.Equal(t, "error", body.Run.Status)
			require.Contains(t, body.Run.Error, "scripted LLM exhausted")
			rows, err := store.LoadMessages(t.Context(), "default", "failed-thread", "")
			require.NoError(t, err)
			require.Len(t, rows, 1)
			require.Contains(t, rows[0].Meta, agentstate.CompletedAtMetaKey)
			failedID := rows[0].RunID

			// A follow-up gets a distinct run and can complete normally.
			model.steps = []scriptedStep{{response: assistantTextResponse("recovered")}}
			success := start()
			require.Contains(t, success.Body.String(), `"type":"RUN_FINISHED"`)
			rows, err = store.LoadMessages(t.Context(), "default", "failed-thread", "")
			require.NoError(t, err)
			require.Len(t, rows, 2)
			require.NotEqual(t, failedID, rows[1].RunID)
		})
	}
}

// JSON-backed brokers must preserve the failed lifecycle discriminator and payload.
func TestFailedChunkRoundTrip(t *testing.T) {
	var chunk responses.ResponseChunk
	require.NoError(t, json.Unmarshal([]byte(`{"type":"run.failed","run_state":{"id":"r","status":"error","error":"provider unavailable"}}`), &chunk))
	require.NotNil(t, chunk.OfRunFailed)
	require.Equal(t, "provider unavailable", chunk.OfRunFailed.RunState.Error)
	data, err := json.Marshal(chunk)
	require.NoError(t, err)
	require.Contains(t, string(data), `"type":"run.failed"`)
}
