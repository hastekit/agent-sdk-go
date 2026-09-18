package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func requestWorkflow(h http.Handler, method, path, namespace, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("X-Namespace", namespace)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func readRun(t *testing.T, w *httptest.ResponseRecorder) runResponse {
	t.Helper()
	var result runResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	return result
}
func testWorkflowHandler(t *testing.T, config HTTPConfig) *HTTPHandler {
	t.Helper()
	registry := NewRegistry()
	require.NoError(t, registry.Register("review", registryGraph(t)))
	config.NamespaceResolver = func(r *http.Request) (string, error) { return r.Header.Get("X-Namespace"), nil }
	return NewHTTPHandler(registry, config)
}
func TestWorkflowHTTPStartStatusResumeAndIsolation(t *testing.T) {
	h := testWorkflowHandler(t, HTTPConfig{RunContextResolver: func(*http.Request) (map[string]any, error) { return map[string]any{"token": "private-token"}, nil }})
	list := requestWorkflow(h, "GET", "/workflows", "a", "")
	require.Equal(t, 200, list.Code)
	require.JSONEq(t, `{"workflows":["review"]}`, list.Body.String())
	started := requestWorkflow(h, "POST", "/workflows/review/runs", "a", `{"input":{"amount":50}}`)
	require.Equal(t, 201, started.Code, started.Body.String())
	run := readRun(t, started)
	require.Equal(t, "paused", run.Status)
	require.Len(t, run.Interrupts, 1)
	require.Equal(t, run.RunID, started.Header().Get("X-Workflow-Run-ID"))
	require.NotContains(t, started.Body.String(), "private-token")
	path := "/workflows/review/runs/" + run.RunID
	require.Equal(t, 404, requestWorkflow(h, "GET", path, "b", "").Code)
	require.Equal(t, 404, requestWorkflow(h, "POST", path+"/resume", "b", `{"resolutions":[]}`).Code)
	require.Equal(t, 200, requestWorkflow(h, "GET", path, "a", "").Code)
	require.Equal(t, 400, requestWorkflow(h, "POST", path+"/resume", "a", `{"resolutions":[{"call_id":"wrong","action":"approve"}]}`).Code)
	require.Equal(t, "paused", readRun(t, requestWorkflow(h, "GET", path, "a", "")).Status)
	body := `{"resolutions":[{"call_id":"` + run.Interrupts[0].FunctionCallMessage.CallID + `","action":"approve"}]}`
	done := requestWorkflow(h, "POST", path+"/resume", "a", body)
	require.Equal(t, 200, done.Code, done.Body.String())
	require.Equal(t, "completed", readRun(t, done).Status)
	require.Contains(t, done.Body.String(), `"finish":50`)
	require.NotContains(t, done.Body.String(), "private-token")
	require.Equal(t, 409, requestWorkflow(h, "POST", path+"/resume", "a", body).Code)
}
func TestWorkflowHTTPRejectsClientCheckpoints(t *testing.T) {
	h := testWorkflowHandler(t, HTTPConfig{})
	for _, body := range []string{`{"input":{ },"status":{"review":"completed"}}`, `{"input":{},"namespace":"other"}`, `{"input":{}} {}`, `{"input":null}`, `{}`} {
		w := requestWorkflow(h, "POST", "/workflows/review/runs", "a", body)
		require.Equal(t, 400, w.Code, body)
	}
	require.Equal(t, 404, requestWorkflow(h, "POST", "/workflows/missing/runs", "a", `{"input":{}}`).Code)
	denied := NewHTTPHandler(NewRegistry(), HTTPConfig{NamespaceResolver: func(*http.Request) (string, error) { return "", errors.New("secret") }})
	w := requestWorkflow(denied, "GET", "/workflows", "", "")
	require.Equal(t, 403, w.Code)
	require.NotContains(t, w.Body.String(), "secret")
}
func TestWorkflowHTTPRunLimitsAndExpiry(t *testing.T) {
	h := testWorkflowHandler(t, HTTPConfig{MaxRuns: 1})
	first := requestWorkflow(h, "POST", "/workflows/review/runs", "", `{"input":{}}`)
	require.Equal(t, 201, first.Code)
	id := readRun(t, first).RunID
	require.Equal(t, 503, requestWorkflow(h, "POST", "/workflows/review/runs", "", `{"input":{}}`).Code)
	h.runs[id].updated = time.Now().Add(-2 * time.Hour)
	require.Equal(t, 404, requestWorkflow(h, "GET", "/workflows/review/runs/"+id, "", "").Code)
	require.Equal(t, 201, requestWorkflow(h, "POST", "/workflows/review/runs", "", `{"input":{}}`).Code)
}
func TestWorkflowHTTPConcurrentResume(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	compiled := registryGraph(t)
	compiled.Nodes["finish"] = cancellationNode(func(ctx context.Context, in *Input) (map[string]any, string, error) {
		close(entered)
		select {
		case <-release:
			return map[string]any{"done": true}, DefaultPort, nil
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	})
	registry := NewRegistry()
	require.NoError(t, registry.Register("review", compiled))
	h := NewHTTPHandler(registry, HTTPConfig{})
	started := readRun(t, requestWorkflow(h, "POST", "/workflows/review/runs", "", `{"input":{}}`))
	path := "/workflows/review/runs/" + started.RunID
	body := `{"resolutions":[{"call_id":"` + started.Interrupts[0].FunctionCallMessage.CallID + `","action":"approve"}]}`
	finished := make(chan *httptest.ResponseRecorder, 1)
	go func() { finished <- requestWorkflow(h, "POST", path+"/resume", "", body) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("execution did not start")
	}
	require.Equal(t, "running", readRun(t, requestWorkflow(h, "GET", path, "", "")).Status)
	conflict := requestWorkflow(h, "POST", path+"/resume", "", body)
	close(release)
	require.Equal(t, 409, conflict.Code)
	require.Equal(t, 200, (<-finished).Code)
}

func TestWorkflowHTTPInvalidFormRemainsPaused(t *testing.T) {
	registry := NewRegistry()
	compiled, err := LoadYAML([]byte(`version: 1
id: form
nodes:
 - id: form
   type: human
   config:
     message: Name
     schema: {type: object, properties: {name: {type: string}}, required: [name]}
`), Dependencies{})
	require.NoError(t, err)
	require.NoError(t, registry.Register("form", compiled))
	h := NewHTTPHandler(registry, HTTPConfig{})
	paused := readRun(t, requestWorkflow(h, "POST", "/workflows/form/runs", "", `{"input":{}}`))
	path := "/workflows/form/runs/" + paused.RunID
	prefix := `{"resolutions":[{"call_id":"` + paused.Interrupts[0].FunctionCallMessage.CallID + `","action":"approve","content":`
	bad := requestWorkflow(h, "POST", path+"/resume", "", prefix+`{"name":5}}]}`)
	require.Equal(t, 400, bad.Code, bad.Body.String())
	require.Equal(t, "paused", readRun(t, requestWorkflow(h, "GET", path, "", "")).Status)
	done := requestWorkflow(h, "POST", path+"/resume", "", prefix+`{"name":"Ada"}}]}`)
	require.Equal(t, 200, done.Code, done.Body.String())
	require.Equal(t, "completed", readRun(t, done).Status)
}
