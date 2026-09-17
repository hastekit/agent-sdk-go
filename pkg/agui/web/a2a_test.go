package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/agui"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

type a2aRuntime func(context.Context, *agents.Agent, *agents.AgentInput) (*agents.AgentOutput, error)

func (f a2aRuntime) Run(ctx context.Context, a *agents.Agent, in *agents.AgentInput) (*agents.AgentOutput, error) {
	return f(ctx, a, in)
}
func (a2aRuntime) RegisterAgent(*agents.AgentOptions) error { return nil }
func (a2aRuntime) StreamBroker() agents.StreamBroker        { return nil }
func a2aOutput(text string) *agents.AgentOutput {
	return &agents.AgentOutput{RunID: "run-1", Status: agentstate.RunStatusCompleted, Output: []responses.InputMessageUnion{{OfOutputMessage: &responses.OutputMessage{Role: constants.RoleAssistant, Content: &responses.OutputContent{{OfOutputText: &responses.OutputTextContent{Text: text}}}}}}}
}

type tenantTransport struct{ tenant string }

func (t tenantTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Tenant", t.tenant)
	return http.DefaultTransport.RoundTrip(r)
}
func a2aClient(t *testing.T, endpoint, tenant string) *a2aclient.Client {
	t.Helper()
	client, err := a2aclient.NewFromEndpoints(t.Context(), []*a2a.AgentInterface{a2a.NewAgentInterface(endpoint, a2a.TransportProtocolJSONRPC)}, a2aclient.WithJSONRPCTransport(&http.Client{Transport: tenantTransport{tenant}}))
	require.NoError(t, err)
	return client
}
func tenantNamespace(r *http.Request) (string, error) { return r.Header.Get("X-Tenant"), nil }

func TestA2ADiscoverySendAndTaskIsolation(t *testing.T) {
	inputs := make(chan *agents.AgentInput, 4)
	runtime := a2aRuntime(func(ctx context.Context, a *agents.Agent, in *agents.AgentInput) (*agents.AgentOutput, error) {
		inputs <- in
		return a2aOutput("hello " + *in.Message.Messages[0].OfEasyInput.Content.OfString), nil
	})
	first := agents.NewAgent(&agents.AgentOptions{Name: "Helper", Runtime: runtime})
	second := agents.NewAgent(&agents.AgentOptions{Name: "Other", Runtime: runtime})
	server := httptest.NewServer(Handler(registry{"Helper": first, "Other": second}, agui.WithNamespaceResolver(tenantNamespace)))
	defer server.Close()
	res, err := http.Get(server.URL + APIPrefix + "/a2a/")
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, 200, res.StatusCode)
	var directory struct {
		Agents []struct{ Name, URL, AgentCardURL string }
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&directory))
	require.Len(t, directory.Agents, 2)
	res, err = http.Get(server.URL + APIPrefix + "/a2a/Helper/.well-known/agent-card.json")
	require.NoError(t, err)
	defer res.Body.Close()
	var card a2a.AgentCard
	require.NoError(t, json.NewDecoder(res.Body).Decode(&card))
	require.Equal(t, "Helper", card.Name)
	require.True(t, card.Capabilities.Streaming)
	endpoint := server.URL + APIPrefix + "/a2a/Helper"
	require.Equal(t, endpoint, card.SupportedInterfaces[0].URL)
	client := a2aClient(t, endpoint, "alice")
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("world"))
	msg.ContextID = "conversation"
	msg.Metadata = map[string]any{"hastekit.skills": map[string]any{"enable": []string{"review"}}}
	result, err := client.SendMessage(t.Context(), &a2a.SendMessageRequest{Message: msg, Tenant: "bob", Metadata: map[string]any{"namespace": "bob"}})
	require.NoError(t, err)
	task, ok := result.(*a2a.Task)
	require.True(t, ok)
	require.Equal(t, a2a.TaskStateCompleted, task.Status.State)
	require.Len(t, task.Artifacts, 1)
	require.Equal(t, "hello world", task.Artifacts[0].Parts[0].Text())
	require.Equal(t, "conversation", task.ContextID)
	input := <-inputs
	require.Equal(t, "alice", input.Namespace)
	require.Equal(t, []string{"review"}, input.Skills.Enable)
	fetched, err := client.GetTask(t.Context(), &a2a.GetTaskRequest{ID: task.ID})
	require.NoError(t, err)
	require.Equal(t, task.ID, fetched.ID)
	listed, err := client.ListTasks(t.Context(), &a2a.ListTasksRequest{})
	require.NoError(t, err)
	require.Len(t, listed.Tasks, 1)
	for _, other := range []*a2aclient.Client{a2aClient(t, endpoint, "bob"), a2aClient(t, server.URL+APIPrefix+"/a2a/Other", "alice")} {
		_, err = other.GetTask(t.Context(), &a2a.GetTaskRequest{ID: task.ID})
		require.Error(t, err)
		_, err = other.CancelTask(t.Context(), &a2a.CancelTaskRequest{ID: task.ID})
		require.Error(t, err)
		list, err := other.ListTasks(t.Context(), &a2a.ListTasksRequest{})
		require.NoError(t, err)
		require.Empty(t, list.Tasks)
	}
	msg = a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("again"))
	msg.ContextID = "conversation"
	_, err = client.SendMessage(t.Context(), &a2a.SendMessageRequest{Message: msg})
	require.NoError(t, err)
	require.Equal(t, input.ThreadID, (<-inputs).ThreadID)
}

func TestA2AStreamingAndFailure(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()
	runtime := a2aRuntime(func(ctx context.Context, a *agents.Agent, in *agents.AgentInput) (*agents.AgentOutput, error) {
		if *in.Message.Messages[0].OfEasyInput.Content.OfString == "fail" {
			return nil, errors.New("provider failed")
		}
		for _, delta := range []string{"hello ", "world"} {
			err := broker.Publish(ctx, in.StreamID, &responses.ResponseChunk{OfOutputTextDelta: &responses.ChunkOutputText[constants.ChunkTypeOutputTextDelta]{Delta: delta}})
			if err != nil {
				return nil, err
			}
		}
		return a2aOutput("hello world"), nil
	})
	agent := agents.NewAgent(&agents.AgentOptions{Name: "Helper", Runtime: runtime, StreamBroker: broker})
	server := httptest.NewServer(Handler(registry{"Helper": agent}))
	defer server.Close()
	client := a2aClient(t, server.URL+APIPrefix+"/a2a/Helper", "alice")
	updates := 0
	var taskID a2a.TaskID
	var state a2a.TaskState
	for event, err := range client.SendStreamingMessage(t.Context(), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))}) {
		require.NoError(t, err)
		switch ev := event.(type) {
		case *a2a.Task:
			taskID = ev.ID
		case *a2a.TaskArtifactUpdateEvent:
			updates++
		case *a2a.TaskStatusUpdateEvent:
			state = ev.Status.State
		}
	}
	require.GreaterOrEqual(t, updates, 2)
	require.Equal(t, a2a.TaskStateCompleted, state)
	task, err := client.GetTask(t.Context(), &a2a.GetTaskRequest{ID: taskID})
	require.NoError(t, err)
	require.Len(t, task.Artifacts, 1)
	require.Equal(t, "hello world", task.Artifacts[0].Parts[0].Text())
	result, err := client.SendMessage(t.Context(), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("fail"))})
	require.NoError(t, err)
	require.Equal(t, a2a.TaskStateFailed, result.(*a2a.Task).Status.State)
	_, err = client.SendMessage(t.Context(), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewRawPart([]byte("unsupported")))})
	require.Error(t, err)
}

func TestA2ACancelStopsExecution(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()
	entered := make(chan struct{})
	stopped := make(chan struct{})
	agent := agents.NewAgent(&agents.AgentOptions{Name: "Helper", StreamBroker: broker, Runtime: a2aRuntime(func(ctx context.Context, a *agents.Agent, in *agents.AgentInput) (*agents.AgentOutput, error) {
		watch, cancel := agents.StopCancelContext(ctx, broker, in.StreamID)
		defer cancel()
		close(entered)
		<-watch.Done()
		close(stopped)
		return a2aOutput("stopped"), nil
	})})
	server := httptest.NewServer(Handler(registry{"Helper": agent}))
	defer server.Close()
	client := a2aClient(t, server.URL+APIPrefix+"/a2a/Helper", "alice")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result, err := client.SendMessage(ctx, &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("wait")), Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
	require.NoError(t, err)
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("execution did not start")
	}
	task := result.(*a2a.Task)
	canceled, err := client.CancelTask(ctx, &a2a.CancelTaskRequest{ID: task.ID})
	require.NoError(t, err)
	require.Equal(t, a2a.TaskStateCanceled, canceled.Status.State)
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("execution did not stop")
	}
}

func TestA2AInputRequiredAndResume(t *testing.T) {
	var calls atomic.Int32
	agent := agents.NewAgent(&agents.AgentOptions{Name: "Helper", Runtime: a2aRuntime(func(ctx context.Context, a *agents.Agent, in *agents.AgentInput) (*agents.AgentOutput, error) {
		if calls.Add(1) == 1 {
			return &agents.AgentOutput{Status: agentstate.RunStatusPaused, RunID: "paused-run", Interrupts: []responses.Interrupt{{FunctionCallMessage: responses.FunctionCallMessage{CallID: "call-1"}, Mode: responses.InterruptModeApproval}}}, nil
		}
		if in.PreviousRunID != "paused-run" {
			return nil, errors.New("missing previous run")
		}
		resolution := in.Message.Messages[0].OfFunctionCallInterruptResolution
		if resolution == nil || resolution.Resolutions[0].CallID != "call-1" {
			return nil, errors.New("missing resolution")
		}
		return a2aOutput("approved"), nil
	})})
	server := httptest.NewServer(Handler(registry{"Helper": agent}))
	defer server.Close()
	client := a2aClient(t, server.URL+APIPrefix+"/a2a/Helper", "alice")
	result, err := client.SendMessage(t.Context(), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("start"))})
	require.NoError(t, err)
	task := result.(*a2a.Task)
	require.Equal(t, a2a.TaskStateInputRequired, task.Status.State)
	require.NotNil(t, task.Status.Message)
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewDataPart(map[string]any{"type": "hastekit.interrupt_response", "resolutions": []map[string]any{{"call_id": "call-1", "action": "approve"}}}))
	msg.TaskID = task.ID
	result, err = client.SendMessage(t.Context(), &a2a.SendMessageRequest{Message: msg})
	require.NoError(t, err)
	require.Equal(t, a2a.TaskStateCompleted, result.(*a2a.Task).Status.State)
	require.Equal(t, task.ID, result.(*a2a.Task).ID)
}

func TestA2ARejectsUnknownAgentAndNamespaceFailures(t *testing.T) {
	agent := agents.NewAgent(&agents.AgentOptions{Name: "Helper"})
	for _, tc := range []struct {
		name    string
		options []agui.Option
		path    string
		want    int
	}{
		{"unknown", nil, "/a2a/missing/.well-known/agent-card.json", 404},
		{"forbidden", []agui.Option{agui.WithNamespaceResolver(func(*http.Request) (string, error) { return "", errors.New("unauthorized") })}, "/a2a/Helper/.well-known/agent-card.json", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			Handler(registry{"Helper": agent}, tc.options...).ServeHTTP(w, httptest.NewRequest("GET", APIPrefix+tc.path, nil))
			require.Equal(t, tc.want, w.Code)
		})
	}
	h := Handler(registry{"Helper": agent}, agui.WithA2ABaseURL("https://agents.example.com/proxy/"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", APIPrefix+"/a2a/Helper/.well-known/agent-card.json", nil))
	var card a2a.AgentCard
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &card))
	require.Equal(t, "https://agents.example.com/proxy/api/agui/a2a/Helper", card.SupportedInterfaces[0].URL)
}

func TestA2ASubscribeToRunningTask(t *testing.T) {
	release := make(chan struct{})
	agent := agents.NewAgent(&agents.AgentOptions{Name: "Helper", Runtime: a2aRuntime(func(ctx context.Context, a *agents.Agent, in *agents.AgentInput) (*agents.AgentOutput, error) {
		select {
		case <-release:
			return a2aOutput("finished"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})})
	server := httptest.NewServer(Handler(registry{"Helper": agent}))
	defer server.Close()
	client := a2aClient(t, server.URL+APIPrefix+"/a2a/Helper", "alice")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result, err := client.SendMessage(ctx, &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("start")), Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
	require.NoError(t, err)
	task := result.(*a2a.Task)
	released := false
	var state a2a.TaskState
	for event, err := range client.SubscribeToTask(ctx, &a2a.SubscribeToTaskRequest{ID: task.ID}) {
		require.NoError(t, err)
		if _, ok := event.(*a2a.Task); ok && !released {
			close(release)
			released = true
		}
		if update, ok := event.(*a2a.TaskStatusUpdateEvent); ok {
			state = update.Status.State
		}
	}
	require.Equal(t, a2a.TaskStateCompleted, state)
}

func TestA2AJSONContentAndOutput(t *testing.T) {
	agent := agents.NewAgent(&agents.AgentOptions{Name: "Helper", Output: map[string]any{"type": "object"}, Runtime: a2aRuntime(func(ctx context.Context, a *agents.Agent, in *agents.AgentInput) (*agents.AgentOutput, error) {
		if *in.Message.Messages[0].OfEasyInput.Content.OfString != `{"question":"hello"}` {
			return nil, errors.New("lost JSON input")
		}
		if in.RunContext["Header"].(map[string]string)["X_Tenant"] != "alice" {
			return nil, errors.New("missing caller header")
		}
		return a2aOutput(`{"answer":"world"}`), nil
	})})
	server := httptest.NewServer(Handler(registry{"Helper": agent}))
	defer server.Close()
	client := a2aClient(t, server.URL+APIPrefix+"/a2a/Helper", "alice")
	result, err := client.SendMessage(t.Context(), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewDataPart(map[string]any{"question": "hello"}))})
	require.NoError(t, err)
	task := result.(*a2a.Task)
	require.Equal(t, a2a.TaskStateCompleted, task.Status.State)
	require.Equal(t, map[string]any{"answer": "world"}, task.Artifacts[0].Parts[0].Data())
}
