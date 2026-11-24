package agents_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

func deferredTool(name, description string) agents.Tool {
	// fakeTool (from agent_test.go) supplies the Execute method BaseTool
	// lacks; ToolSearch only ever reads the tool's descriptor, never calls
	// Execute, so the tool body is irrelevant here.
	return &fakeTool{
		BaseTool: &agents.BaseTool{
			ToolUnion: responses.ToolUnion{
				OfFunction: &responses.FunctionTool{
					Name:        name,
					Description: utils.Ptr(description),
					Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
				},
			},
			Deferred: true,
		},
	}
}

func runToolSearch(t *testing.T, tools []agents.Tool, args string) *agents.ToolCallResponse {
	t.Helper()
	return runToolSearchWithState(t, tools, args, nil)
}

// runToolSearchWithState runs ToolSearch with state as the run's state
// snapshot, the way the loop hands it what earlier calls wrote.
func runToolSearchWithState(t *testing.T, tools []agents.Tool, args string, state map[string]string) *agents.ToolCallResponse {
	t.Helper()
	search := agents.NewToolSearchTool(tools)
	resp, err := search.Execute(context.Background(), &agents.ToolCall{
		FunctionCallMessage: &responses.FunctionCallMessage{
			ID:        "fc_1",
			CallID:    "call_1",
			Name:      "ToolSearch",
			Arguments: args,
		},
		State: state,
	})
	if err != nil {
		t.Fatalf("ToolSearch execute failed: %v", err)
	}
	return resp
}

// A plain keyword query — the form the tool's schema invites and the one
// the specialist emitted during handoff — must activate the matching
// deferred tool rather than silently no-op.
func TestToolSearch_KeywordQueryActivatesTool(t *testing.T) {
	tools := []agents.Tool{
		deferredTool("get_user_name_by_id", "Look up a user's name from their id"),
		deferredTool("send_email", "Send an email to a recipient"),
	}

	resp := runToolSearch(t, tools, `{"query":"get_user_name_by_id","max_results":1}`)

	if got := resp.StateUpdates["activated_deferred_tools"]; got != "get_user_name_by_id" {
		t.Fatalf("activated = %q, want get_user_name_by_id", got)
	}
	if out := *resp.FunctionCallOutputMessage.Output.OfString; !strings.Contains(out, "get_user_name_by_id") {
		t.Fatalf("output missing activated tool: %q", out)
	}
}

// Keyword matching also works against a word from the description, and
// respects max_results ranking by relevance.
func TestToolSearch_KeywordMatchesDescriptionAndRanks(t *testing.T) {
	tools := []agents.Tool{
		deferredTool("send_email", "Send an email to a user"),
		deferredTool("get_user_name_by_id", "Look up a user's name"),
		deferredTool("delete_account", "Remove an account"),
	}

	// "user" appears in the first two tools' text; max_results caps to 2.
	resp := runToolSearch(t, tools, `{"query":"user","max_results":2}`)

	activated := strings.Split(resp.StateUpdates["activated_deferred_tools"], ",")
	if len(activated) != 2 {
		t.Fatalf("activated %d tools, want 2: %v", len(activated), activated)
	}
	for _, name := range activated {
		if name == "delete_account" {
			t.Fatalf("delete_account should not match query 'user': %v", activated)
		}
	}
}

func TestToolSearch_SelectQueryStillWorks(t *testing.T) {
	tools := []agents.Tool{
		deferredTool("get_user_name_by_id", "Look up a user's name"),
		deferredTool("send_email", "Send an email"),
	}

	resp := runToolSearch(t, tools, `{"query":"select:get_user_name_by_id"}`)

	if got := resp.StateUpdates["activated_deferred_tools"]; got != "get_user_name_by_id" {
		t.Fatalf("activated = %q, want get_user_name_by_id", got)
	}
}

func TestToolSearch_NoMatchActivatesNothing(t *testing.T) {
	tools := []agents.Tool{deferredTool("send_email", "Send an email")}

	resp := runToolSearch(t, tools, `{"query":"weather forecast"}`)

	if got := resp.StateUpdates["activated_deferred_tools"]; got != "" {
		t.Fatalf("activated = %q, want empty", got)
	}
	if out := *resp.FunctionCallOutputMessage.Output.OfString; !strings.Contains(out, "No matching") {
		t.Fatalf("output should report no match, got: %q", out)
	}
}

// A second search adds to what the first activated rather than replacing it:
// the loop copies StateUpdates over the run's state, so a search that wrote
// only its own names would drop the first search's tools.
func TestToolSearch_LaterSearchKeepsEarlierActivations(t *testing.T) {
	tools := []agents.Tool{
		deferredTool("get_user_name_by_id", "Look up a user's name"),
		deferredTool("send_email", "Send an email"),
	}

	first := runToolSearch(t, tools, `{"query":"select:get_user_name_by_id"}`)
	second := runToolSearchWithState(t, tools, `{"query":"email"}`, first.StateUpdates)

	if got := second.StateUpdates["activated_deferred_tools"]; got != "get_user_name_by_id,send_email" {
		t.Fatalf("activated = %q, want get_user_name_by_id,send_email", got)
	}
	// The output reports what this call activated, not the running total.
	if out := *second.FunctionCallOutputMessage.Output.OfString; out != "Activated tools: send_email" {
		t.Fatalf("output = %q, want only this call's tools", out)
	}
}

func TestToolSearch_RepeatedActivationIsNotDuplicated(t *testing.T) {
	tools := []agents.Tool{
		deferredTool("get_user_name_by_id", "Look up a user's name"),
		deferredTool("send_email", "Send an email"),
	}
	state := map[string]string{"activated_deferred_tools": "get_user_name_by_id,send_email"}

	resp := runToolSearchWithState(t, tools, `{"query":"select:send_email,get_user_name_by_id"}`, state)

	if got := resp.StateUpdates["activated_deferred_tools"]; got != "get_user_name_by_id,send_email" {
		t.Fatalf("activated = %q, want get_user_name_by_id,send_email", got)
	}
}

// A search that matches nothing must not clear what earlier searches activated.
func TestToolSearch_NoMatchKeepsEarlierActivations(t *testing.T) {
	tools := []agents.Tool{deferredTool("send_email", "Send an email")}
	state := map[string]string{"activated_deferred_tools": "send_email"}

	resp := runToolSearchWithState(t, tools, `{"query":"weather forecast"}`, state)

	if got := resp.StateUpdates["activated_deferred_tools"]; got != "send_email" {
		t.Fatalf("activated = %q, want send_email", got)
	}
}

// Through the agent loop: after a select: search and then a keyword search,
// the next model request carries both deferred tools.
func TestAgentLoop_ToolSearchActivationsAccumulateAcrossSearches(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_search_1", "ToolSearch", `{"query":"select:get_user_name_by_id"}`),
		toolCallResponse("call_search_2", "ToolSearch", `{"query":"email"}`),
		textResponse("done"),
	}}
	tools := []agents.Tool{
		deferredTool("get_user_name_by_id", "Look up a user's name"),
		deferredTool("send_email", "Send an email"),
	}
	agent := newScriptedAgent("main", llm, nil, nil, tools, nil)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test",
		ThreadID:  "thread-tool-search",
		Message:   userMessage("look up the user, then email them"),
	})
	requireStatus(t, out, agentstate.RunStatusCompleted)

	if llm.callCount() != 3 {
		t.Fatalf("model called %d times, want 3", llm.callCount())
	}
	wantByCall := [][]string{
		nil,
		{"get_user_name_by_id"},
		{"get_user_name_by_id", "send_email"},
	}
	for i, want := range wantByCall {
		got := deferredToolNames(llm.request(i).Tools)
		if !slices.Equal(got, want) {
			t.Fatalf("request %d deferred tools = %v, want %v", i, got, want)
		}
	}
}

// deferredToolNames is the names of the request's tools that come from the
// deferred set used in these tests, in request order.
func deferredToolNames(tools []responses.ToolUnion) []string {
	var names []string
	for _, tool := range tools {
		if tool.OfFunction == nil {
			continue
		}
		switch tool.OfFunction.Name {
		case "get_user_name_by_id", "send_email":
			names = append(names, tool.OfFunction.Name)
		}
	}
	return names
}
