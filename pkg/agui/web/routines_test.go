package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agui"
	"github.com/hastekit/agent-sdk-go/pkg/routines"
	"github.com/stretchr/testify/require"
)

func TestRoutineAPIsThroughWebHandler(t *testing.T) {
	reg := registry{"Helper": agents.NewAgent(&agents.AgentOptions{Name: "Helper"})}
	store, err := routines.OpenJSONLStore(filepath.Join(t.TempDir(), "routines.jsonl"))
	require.NoError(t, err)
	defer store.Close()
	service := routines.NewService(store, &routines.AgentExecutor{Registry: reg})
	calls := 0
	var routeID string
	handler := Handler(reg, agui.WithRoutines(service), agui.WithNamespaceResolver(func(r *http.Request) (string, error) {
		calls++
		routeID = r.PathValue("id")
		if r.Header.Get("X-Tenant") == "denied" {
			return "", errors.New("denied")
		}
		return r.Header.Get("X-Tenant"), nil
	}))
	request := func(method, path, tenant, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, APIPrefix+path, bytes.NewBufferString(body))
		req.Header.Set("X-Tenant", tenant)
		result := httptest.NewRecorder()
		before := calls
		handler.ServeHTTP(result, req)
		require.Equal(t, before+1, calls, "namespace must resolve once per request")
		return result
	}
	agentsResponse := request("GET", "/agents", "tenant-a", "")
	require.Contains(t, agentsResponse.Body.String(), `"routines":true`)
	require.JSONEq(t, `["Helper"]`, request("GET", "/routines/agents", "tenant-a", "").Body.String())
	created := request("POST", "/routines", "tenant-a", `{"name":"Daily report","agent":"Helper","instruction":"Summarize the day","schedule":{"cron":"0 9 * * *","timezone":"UTC"}}`)
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var routine routines.Routine
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &routine))
	require.Equal(t, "tenant-a", routine.Namespace)
	path := "/routines/" + routine.ID
	require.Equal(t, http.StatusOK, request("GET", path, "tenant-a", "").Code)
	require.Equal(t, routine.ID, routeID, "resolver must see the matched routine ID")
	require.Equal(t, http.StatusNotFound, request("GET", path, "tenant-b", "").Code)
	require.NotContains(t, request("GET", "/routines", "tenant-b", "").Body.String(), routine.ID)
	require.Equal(t, http.StatusForbidden, request("POST", path+"/pause", "denied", "").Code)
	paused := request("POST", path+"/pause", "tenant-a", "")
	require.Equal(t, http.StatusOK, paused.Code)
	require.Contains(t, paused.Body.String(), `"enabled":false`)
	require.Contains(t, request("POST", path+"/resume", "tenant-a", "").Body.String(), `"enabled":true`)
	require.Equal(t, http.StatusBadRequest, request("POST", "/routines", "tenant-a", `{"name":"Bad","agent":"Helper","instruction":"hi","schedule":{"cron":"invalid"}}`).Code)
	require.Equal(t, http.StatusNoContent, request("DELETE", path, "tenant-a", "").Code)
	require.Equal(t, http.StatusNotFound, request("GET", path, "tenant-a", "").Code)
}

func TestRoutinesDisabledByDefault(t *testing.T) {
	handler := Handler(registry{})
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, httptest.NewRequest("GET", APIPrefix+"/agents", nil))
	require.Contains(t, result.Body.String(), `"routines":false`)
	result = httptest.NewRecorder()
	handler.ServeHTTP(result, httptest.NewRequest("GET", APIPrefix+"/routines", nil))
	require.Equal(t, http.StatusNotFound, result.Code)
}

func TestRoutineConversationHistoryAcrossAgents(t *testing.T) {
	ctx := context.Background()
	persistence := history.NewInMemoryConversationPersistence()
	reg := registry{}
	for _, name := range []string{"OldAgent", "NewAgent"} {
		reg[name] = agents.NewAgent(&agents.AgentOptions{Name: name, History: history.NewConversationManager(persistence)})
	}
	store, err := routines.OpenJSONLStore(filepath.Join(t.TempDir(), "routines.jsonl"))
	require.NoError(t, err)
	defer store.Close()
	service := routines.NewService(store, &routines.AgentExecutor{Registry: reg})
	routine, err := service.Create(ctx, "tenant", routines.Definition{Name: "Report", Agent: "NewAgent", Instruction: "Summarize", Schedule: routines.Schedule{Cron: "0 9 * * *"}})
	require.NoError(t, err)
	save := func(ns, run, previous, thread, routineID, agent string) {
		t.Helper()
		require.NoError(t, persistence.SaveMessages(ctx, ns,

			routineID, run, previous, thread, "", nil, map[string]any{
				history.RunContextMetaKey: map[string]any{history.RoutineIDContextKey: routineID, history.RoutineAgentContextKey: agent},
			}))
	}
	save("tenant", "old", "", "old-thread", routine.ID, "OldAgent")
	save("tenant", "latest", "", "latest-thread", routine.ID, "NewAgent")
	// Editing an older run must not make it the latest scheduled occurrence.
	save("tenant", "followup", "old", "old-thread", "", "")
	save("tenant", "normal", "", "normal-thread", "", "")
	save("tenant", "unrelated", "", "unrelated-thread", "other-routine", "NewAgent")
	save("other", "private", "", "private-thread", routine.ID, "NewAgent")
	handler := Handler(reg, agui.WithRoutines(service), agui.WithNamespaceResolver(func(r *http.Request) (string, error) { return r.Header.Get("X-Tenant"), nil }))
	request := func(path, ns string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", APIPrefix+path, nil)
		r.Header.Set("X-Tenant", ns)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	response := request("/routines/"+routine.ID+"/threads", "tenant")
	require.Equal(t, 200, response.Code, response.Body.String())
	var body struct {
		Threads []history.ThreadInfo `json:"threads"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.Len(t, body.Threads, 2, "shared stores must not duplicate runs")
	require.Equal(t, "latest-thread", body.Threads[0].ThreadID)
	require.Equal(t, "NewAgent", body.Threads[0].AgentName)
	require.Equal(t, "old-thread", body.Threads[1].ThreadID)
	require.Equal(t, "OldAgent", body.Threads[1].AgentName)
	require.Equal(t, 404, request("/routines/"+routine.ID+"/threads", "other").Code)
	response = request("/agents/NewAgent/threads", "tenant")
	require.NotContains(t, response.Body.String(), `"group_id":"`+routine.ID+`"`)
	require.Contains(t, response.Body.String(), "normal-thread")
	require.NotContains(t, response.Body.String(), "private-thread")
	response = request("/agents/NewAgent/threads?group_id="+routine.ID, "tenant")
	require.Equal(t, 200, response.Code)
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.Len(t, body.Threads, 2)
	for _, thread := range body.Threads {
		require.Equal(t, routine.ID, thread.GroupID)
	}
	require.NotContains(t, response.Body.String(), "normal-thread")
}
