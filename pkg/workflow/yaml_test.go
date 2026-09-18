package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

func TestYAMLAPIBranchesAndCode(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "POST", r.Method)
		require.Equal(t, "secret", r.Header.Get("X-Token"))
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.EqualValues(t, 3, body["count"])
		_, _ = w.Write([]byte(`{"count":3}`))
	}))
	defer server.Close()
	compiled, err := LoadYAML([]byte(fmt.Sprintf(`
version: 1
id: demo
nodes:
  - id: api
    type: api
    config:
      url: %s
      method: POST
      headers: {X-Token: '${{ input.token }}'}
      body: {count: '${{ input.count }}'}
  - id: transform
    type: javascript
    config:
      code: 'input.count = 999; return {count: nodes.api.body.count * 2};'
  - id: condition
    type: if_else
    config: {condition: 'nodes.transform.count === 6'}
  - id: route
    type: switch
    config:
      value: '${{ nodes.transform.count }}'
      cases: [{value: 6, port: six}]
  - id: wait
    type: delay
    config: {duration: 0s}
  - id: wrong
    type: javascript
    config: {code: 'throw new Error("wrong branch");'}
edges:
  - {from: START, to: api}
  - {from: api, to: transform}
  - {from: transform, to: condition}
  - {from: condition, port: 'true', to: route}
  - {from: condition, port: 'false', to: wrong}
  - {from: route, port: six, to: wait}
  - {from: route, to: wrong}
`, server.URL)), Dependencies{})
	require.NoError(t, err)
	in := &Input{RunID: "r", RunContext: map[string]any{"input": map[string]any{"count": 3, "token": "secret"}}}
	out, err := compiled.Execute(context.Background(), in)
	require.NoError(t, err)
	require.Equal(t, NodeStatusCompleted, out.Status["wait"])
	require.Equal(t, NodeStatusSkipped, out.Status["wrong"])
	require.Equal(t, 3, in.RunContext["input"].(map[string]any)["count"], "JavaScript must not mutate shared input")
	require.EqualValues(t, 1, calls.Load())
}

func TestYAMLValidation(t *testing.T) {
	for _, descriptor := range []string{
		`version: 2`,
		"version: 1\nid: x\nunknown: true",
		"version: 1\nid: x\nnodes: [{id: x, type: unknown}]",
		"version: 1\nid: x\nnodes: [{id: x, type: delay, config: {duration: -1s}}]",
		"version: 1\nid: x\nnodes: [{id: x, type: delay, config: {duration: 1s, typo: true}}]",
		"version: 1\nid: x\nnodes: [{id: x, type: agent, config: {agent: missing, message: hi}}]",
		"version: 1\nid: x\nnodes: [{id: x, type: javascript, config: {code: 'return {'}}]",
		"version: 1\nid: x\nnodes: [{id: x, type: delay, config: {duration: 0s}}]\nedges: [{from: x, to: x}]",
		"version: 1\nid: x\nnodes: [{id: x, type: delay, config: {duration: 0s}}]\nedges: [{from: x, port: bogus, to: END}]",
		"version: 1\nid: x\n---\nversion: 1",
	} {
		_, err := LoadYAML([]byte(descriptor), Dependencies{})
		require.Error(t, err, descriptor)
	}
}

func TestYAMLCancellation(t *testing.T) {
	for _, node := range []string{
		`type: delay
    config: {duration: 1h}`,
		`type: javascript
    config: {code: 'while (true) {}'}`,
	} {
		compiled, err := LoadYAML([]byte("version: 1\nid: cancel\nnodes:\n  - id: work\n    "+node), Dependencies{JavaScriptTimeout: 20 * time.Millisecond})
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		_, err = compiled.Execute(ctx, nil)
		cancel()
		require.ErrorIs(t, err, context.DeadlineExceeded)
	}
}

func workflowCall() *agents.ToolCall {
	return &agents.ToolCall{FunctionCallMessage: &responses.FunctionCallMessage{ID: "call", CallID: "call", Name: "process", Arguments: `{"amount":10}`}, Namespace: "tenant", State: map[string]string{}}
}
func continueTool(call *agents.ToolCall, result *agents.ToolCallResponse, action string) {
	for k, v := range result.StateUpdates {
		call.State[k] = v
	}
	call.ShouldResume = true
	call.ResumeMessages = []responses.InputMessageUnion{{OfFunctionCallInterruptResolution: &responses.FunctionCallInterruptResolutionMessage{Resolutions: []responses.InterruptResolution{{CallID: result.Interrupts[0].FunctionCallMessage.CallID, Action: action, Content: json.RawMessage(`{"name":"Ada"}`)}}}}}
}
func TestWorkflowToolPausesAndResumesWithoutRepeatingAPI(t *testing.T) {
	for _, action := range []string{"approve", "reject"} {
		t.Run(action, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = w.Write([]byte(`{}`)) }))
			defer server.Close()
			compiled, err := LoadYAML([]byte(fmt.Sprintf(`
version: 1
id: approval
nodes:
  - {id: before, type: api, config: {url: '%s'}}
  - id: human
    type: human
    config:
      message: Review this
      schema: {type: object, properties: {name: {type: string}}, required: [name]}
  - {id: yes, type: javascript, config: {code: 'return nodes.human.content.name;'}}
  - {id: no, type: javascript, config: {code: 'return "rejected";'}}
edges:
  - {from: before, to: human}
  - {from: human, port: approved, to: yes}
  - {from: human, port: rejected, to: no}
`, server.URL)), Dependencies{})
			require.NoError(t, err)
			tool, err := NewTool("process", "Review", nil, compiled)
			require.NoError(t, err)
			call := workflowCall()
			paused, err := tool.Execute(context.Background(), call)
			require.NoError(t, err)
			require.Len(t, paused.Interrupts, 1)
			require.Nil(t, paused.FunctionCallOutputMessage)
			require.Equal(t, responses.InterruptModeForm, paused.Interrupts[0].Mode)
			continueTool(call, paused, action)
			// A newly constructed wrapper resumes entirely from serialized tool state.
			tool, err = NewTool("process", "Review", nil, compiled)
			require.NoError(t, err)
			done, err := tool.Execute(context.Background(), call)
			require.NoError(t, err)
			require.Empty(t, done.Interrupts)
			require.NotNil(t, done.FunctionCallOutputMessage)
			want := "Ada"
			if action == "reject" {
				want = "rejected"
			}
			require.Contains(t, *done.Output.OfString, want)
			require.EqualValues(t, 1, calls.Load())
		})
	}
}

type workflowAgentFunc func(context.Context, *agents.AgentInput) (*agents.AgentOutput, error)

func (f workflowAgentFunc) Run(ctx context.Context, in *agents.AgentInput) (*agents.AgentOutput, error) {
	return f(ctx, in)
}

type workflowMCP struct{ tool agents.Tool }

func (workflowMCP) GetName() string { return "test" }
func (m workflowMCP) ListTools(context.Context, map[string]any) ([]agents.Tool, error) {
	return []agents.Tool{m.tool}, nil
}

type workflowMCPTool struct {
	agents.BaseTool
	calls int
}

func (t *workflowMCPTool) Execute(_ context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
	t.calls++
	if !call.ShouldResume {
		return &agents.ToolCallResponse{StateUpdates: map[string]string{"request": "saved"}, Interrupts: []responses.Interrupt{{FunctionCallMessage: *call.FunctionCallMessage, Mode: responses.InterruptModeApproval}}}, nil
	}
	if call.State["request"] != "saved" {
		return nil, fmt.Errorf("lost MCP continuation")
	}
	text := "mcp complete"
	return &agents.ToolCallResponse{FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{CallID: call.CallID, Output: responses.FunctionCallOutputContentUnion{OfString: &text}}}, nil
}
func TestWorkflowAgentAndMCPContinuations(t *testing.T) {
	agentCalls := 0
	thread := ""
	agent := workflowAgentFunc(func(ctx context.Context, in *agents.AgentInput) (*agents.AgentOutput, error) {
		agentCalls++
		require.Equal(t, "tenant", in.Namespace)
		if agentCalls == 1 {
			thread = in.ThreadID
			return &agents.AgentOutput{RunID: "child-run", Status: agentstate.RunStatusPaused, Interrupts: []responses.Interrupt{{FunctionCallMessage: responses.FunctionCallMessage{CallID: "child-call", Name: "child"}, Mode: responses.InterruptModeApproval}}}, nil
		}
		require.Equal(t, thread, in.ThreadID)
		require.Equal(t, "child-run", in.PreviousRunID)
		require.NotNil(t, in.Message.Messages[0].OfFunctionCallInterruptResolution)
		return &agents.AgentOutput{RunID: "child-done", Status: agentstate.RunStatusCompleted}, nil
	})
	mcpTool := &workflowMCPTool{BaseTool: agents.BaseTool{ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{Name: "lookup"}}}}
	compiled, err := LoadYAML([]byte(`
version: 1
id: nested
nodes:
 - {id: child, type: agent, config: {agent: helper, message: '${{ "Amount " + input.amount }}'}}
 - {id: lookup, type: mcp, config: {server: test, tool: lookup, arguments: {amount: '${{ input.amount }}'}}}
edges: [{from: child, to: lookup}]
`), Dependencies{Agents: map[string]AgentRunner{"helper": agent}, MCPServers: map[string]agents.MCPToolset{"test": workflowMCP{mcpTool}}})
	require.NoError(t, err)
	tool, err := NewTool("process", "", nil, compiled)
	require.NoError(t, err)
	call := workflowCall()
	for range 2 {
		paused, err := tool.Execute(context.Background(), call)
		require.NoError(t, err)
		require.Len(t, paused.Interrupts, 1)
		continueTool(call, paused, "approve")
	}
	done, err := tool.Execute(context.Background(), call)
	require.NoError(t, err)
	require.Empty(t, done.Interrupts)
	require.Contains(t, *done.Output.OfString, "mcp complete")
	require.Equal(t, 2, agentCalls)
	require.Equal(t, 2, mcpTool.calls)
}

func TestWorkflowParallelPauses(t *testing.T) {
	compiled, err := LoadYAML([]byte(`version: 1
id: parallel
nodes:
 - {id: a, type: human, config: {message: A}}
 - {id: b, type: human, config: {message: B}}
`), Dependencies{})
	require.NoError(t, err)
	tool, err := NewTool("process", "", nil, compiled)
	require.NoError(t, err)
	call := workflowCall()
	ids := map[string]bool{}
	for range 2 {
		paused, err := tool.Execute(context.Background(), call)
		require.NoError(t, err)
		require.Len(t, paused.Interrupts, 1)
		id := paused.Interrupts[0].FunctionCallMessage.CallID
		require.False(t, ids[id])
		ids[id] = true
		continueTool(call, paused, "approve")
	}
	done, err := tool.Execute(context.Background(), call)
	require.NoError(t, err)
	require.Empty(t, done.Interrupts)
	require.NotNil(t, done.FunctionCallOutputMessage)
}

func TestAPIResponseLimitsAndErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "fail") {
			w.WriteHeader(500)
		}
		_, _ = w.Write([]byte("12345"))
	}))
	defer server.Close()
	for _, path := range []string{"/fail", "/large"} {
		compiled, err := LoadYAML([]byte(fmt.Sprintf("version: 1\nid: api\nnodes: [{id: request, type: api, config: {url: '%s%s'}}]", server.URL, path)), Dependencies{MaxResponseBytes: func() int64 {
			if path == "/fail" {
				return 100
			}
			return 4
		}()})
		require.NoError(t, err)
		_, err = compiled.Execute(context.Background(), nil)
		require.Error(t, err)
	}
}

func TestWorkflowToolKeepsApplicationContextPrivate(t *testing.T) {
	compiled, err := LoadYAML([]byte(`version: 1
id: context
nodes:
 - {id: result, type: javascript, config: {code: 'return input.amount;'}}
`), Dependencies{})
	require.NoError(t, err)
	tool, err := NewTool("process", "", map[string]any{"type": "object", "required": []string{"amount"}, "properties": map[string]any{"amount": map[string]any{"type": "number"}}}, compiled)
	require.NoError(t, err)
	call := workflowCall()
	call.RunContext = map[string]any{"token": "secret-credential"}
	out, err := tool.Execute(context.Background(), call)
	require.NoError(t, err)
	require.NotContains(t, *out.Output.OfString, "secret-credential")
	call.Arguments = `{"amount":"wrong"}`
	_, err = tool.Execute(context.Background(), call)
	require.ErrorContains(t, err, "workflow arguments")
	call.ShouldResume = true
	_, err = tool.Execute(context.Background(), call)
	require.ErrorContains(t, err, "checkpoint missing")
}

func TestJavaScriptRejectsAsyncAndInvalidOutput(t *testing.T) {
	for _, code := range []string{`return Promise.resolve(1);`, `return undefined;`, `const x = {}; x.self = x; return x;`, `throw new Error("failed");`} {
		_, err := evalJS(context.Background(), "(function(){"+code+"})()", &Input{RunContext: map[string]any{}}, time.Second)
		require.Error(t, err, code)
	}
}

func TestHumanFormRejectsInvalidContent(t *testing.T) {
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
	state, err := compiled.Execute(context.Background(), nil)
	require.NoError(t, err)
	require.NotNil(t, state.Pause)
	state.SetResume("form", map[string]any{"action": "approve", "content": map[string]any{"name": 5}})
	_, err = compiled.Execute(context.Background(), state)
	require.ErrorContains(t, err, "human response")
}

func TestMCPWorkflowHonorsApprovalBeforeExecution(t *testing.T) {
	mcpTool := &workflowMCPTool{BaseTool: agents.BaseTool{RequiresApproval: true, ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{Name: "lookup"}}}}
	compiled, err := LoadYAML([]byte(`version: 1
id: approval
nodes:
 - {id: call, type: mcp, config: {server: test, tool: lookup}}
`), Dependencies{MCPServers: map[string]agents.MCPToolset{"test": workflowMCP{mcpTool}}})
	require.NoError(t, err)
	tool, err := NewTool("process", "", nil, compiled)
	require.NoError(t, err)
	call := workflowCall()
	paused, err := tool.Execute(context.Background(), call)
	require.NoError(t, err)
	require.Len(t, paused.Interrupts, 1)
	require.Zero(t, mcpTool.calls)
	continueTool(call, paused, "reject")
	done, err := tool.Execute(context.Background(), call)
	require.NoError(t, err)
	require.Contains(t, *done.Output.OfString, "rejected")
	require.Zero(t, mcpTool.calls)
}

func TestAPICallCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	compiled, err := LoadYAML([]byte(fmt.Sprintf("version: 1\nid: cancel\nnodes: [{id: api, type: api, config: {url: '%s'}}]", server.URL)), Dependencies{})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = compiled.Execute(ctx, nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
