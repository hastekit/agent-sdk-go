package agents_test

import (
	"context"
	"strings"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

func deferredTool(name, description string) agents.Tool {
	// fakeTool (from agent_test.go) supplies the Execute method BaseTool
	// lacks; ToolSearch only ever reads Tool()/IsDeferred(), never calls
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
	search := agents.NewToolSearchTool(tools)
	resp, err := search.Execute(context.Background(), &agents.ToolCall{
		FunctionCallMessage: &responses.FunctionCallMessage{
			ID:        "fc_1",
			CallID:    "call_1",
			Name:      "ToolSearch",
			Arguments: args,
		},
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
