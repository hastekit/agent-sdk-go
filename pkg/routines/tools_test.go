package routines

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

func TestSelectedRoutineTools(t *testing.T) {
	ctx := context.Background()
	svc := NewService(testStore(t), &fakeExecutor{})
	factory := Tools(svc)
	selected := []agents.Tool{factory.CreateRoutineTool(), factory.GetRoutineTool(), factory.DeleteRoutineTool()}
	d, _ := json.Marshal(definition())
	call := &agents.ToolCall{Namespace: "tenant", FunctionCallMessage: &responses.FunctionCallMessage{CallID: "call-1", Arguments: fmt.Sprintf(`{"routine":%s}`, d)}}
	result, err := selected[0].Execute(ctx, call)
	if err != nil {
		t.Fatal(err)
	}
	var r Routine
	if err := json.Unmarshal([]byte(*result.Output.OfString), &r); err != nil {
		t.Fatal(err)
	}
	if r.ID == "" || r.Namespace != "tenant" || result.CallID != "call-1" {
		t.Fatalf("routine=%+v output=%+v", r, result)
	}
	call.Arguments = fmt.Sprintf(`{"id":%q}`, r.ID)
	// Model-supplied call names cannot change the operation of the selected tool.
	call.Name = "routines_delete"
	if _, err := selected[1].Execute(ctx, call); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, "tenant", r.ID); err != nil {
		t.Fatal("read tool mutated the routine", err)
	}
	call.Namespace = "other"
	if _, err := selected[2].Execute(ctx, call); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-namespace delete", err)
	}
	call.Namespace = "tenant"
	if _, err := selected[2].Execute(ctx, call); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, "tenant", r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("delete did not remove routine", err)
	}
}

func TestRoutineToolInstancesAreIndependent(t *testing.T) {
	factory := Tools(NewService(testStore(t), &fakeExecutor{}))
	first, second := factory.CreateRoutineTool(), factory.CreateRoutineTool()
	first.GetToolDescriptor().RequiresApproval = true
	first.GetToolDescriptor().ToolUnion.OfFunction.Parameters["properties"].(map[string]any)["routine"].(map[string]any)["type"] = "changed"
	if second.GetToolDescriptor().RequiresApproval {
		t.Fatal("approval setting leaked between instances")
	}
	schema := second.GetToolDescriptor().ToolUnion.OfFunction.Parameters["properties"].(map[string]any)["routine"].(map[string]any)
	if schema["type"] != "object" {
		t.Fatal("schema mutation leaked between instances")
	}
}

func TestRoutineToolSchemaContract(t *testing.T) {
	factory := Tools(nil)
	for _, tool := range []agents.Tool{
		factory.CreateRoutineTool(), factory.UpdateRoutineTool(), factory.GetRoutineTool(),
		factory.DeleteRoutineTool(), factory.PauseRoutineTool(), factory.ResumeRoutineTool(),
		factory.ListRoutinesTool(), factory.ListAgentsTool(), factory.GetRoutineStatusTool(),
	} {
		function := tool.GetToolDescriptor().ToolUnion.OfFunction
		t.Run(function.Name, func(t *testing.T) {
			schema := function.Parameters
			if schema["type"] != "object" || schema["additionalProperties"] != false {
				t.Fatalf("expected closed object schema: %#v", schema)
			}
			data, err := json.Marshal(schema)
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{`"$schema"`, `"$id"`, `"$ref"`, `"$defs"`} {
				if strings.Contains(string(data), key) {
					t.Fatalf("unexpected %s in tool schema: %s", key, data)
				}
			}
			properties := schema["properties"].(map[string]any)
			if len(properties) != lenOrZero(schema["required"]) {
				t.Fatal("all tool arguments must be required")
			}
			if routine, ok := properties["routine"].(map[string]any); ok {
				if lenOrZero(routine["required"]) != 4 || routine["additionalProperties"] != false {
					t.Fatalf("invalid definition schema: %#v", routine)
				}
				fields := routine["properties"].(map[string]any)
				schedule := fields["schedule"].(map[string]any)
				if lenOrZero(schedule["required"]) != 0 {
					t.Fatal("schedule alternatives must remain optional")
				}
				at := schedule["properties"].(map[string]any)["at"].(map[string]any)
				if at["type"] != "string" || at["format"] != "date-time" || at["description"] == nil {
					t.Fatalf("datetime format or description lost: %#v", at)
				}
			}
		})
	}
}

func lenOrZero(value any) int {
	if value == nil {
		return 0
	}
	return len(value.([]any))
}

func TestRoutineStatusToolRequiresScheduler(t *testing.T) {
	svc := NewService(testStore(t), &fakeExecutor{})
	for _, factory := range []*RoutineTools{Tools(svc), Tools(svc, nil)} {
		_, err := factory.GetRoutineStatusTool().Execute(context.Background(), &agents.ToolCall{Namespace: "tenant", FunctionCallMessage: &responses.FunctionCallMessage{Arguments: `{"id":"routine"}`}})
		if err == nil || !strings.Contains(err.Error(), "requires a scheduler") {
			t.Fatal(err)
		}
	}
}

func TestRoutineToolsDecodeOnlyTheirOwnArguments(t *testing.T) {
	factory := Tools(NewService(testStore(t), &fakeExecutor{}))
	cases := []struct {
		tool    agents.Tool
		foreign string
	}{
		{factory.CreateRoutineTool(), `{"id":"unexpected"}`},
		{factory.ListRoutinesTool(), `{"id":"unexpected"}`},
		{factory.GetRoutineTool(), `{"routine":{}}`},
		{factory.UpdateRoutineTool(), `{"operation":"delete"}`},
		{factory.DeleteRoutineTool(), `{"routine":{}}`},
		{factory.PauseRoutineTool(), `{"routine":{}}`},
		{factory.ResumeRoutineTool(), `{"routine":{}}`},
		{factory.ListAgentsTool(), `{"id":"unexpected"}`},
		{factory.GetRoutineStatusTool(), `{"routine":{}}`},
	}
	for _, tc := range cases {
		t.Run(tc.tool.GetToolDescriptor().ToolUnion.OfFunction.Name, func(t *testing.T) {
			if _, err := tc.tool.Execute(context.Background(), nil); err == nil {
				t.Fatal("missing call accepted")
			}
			call := &agents.ToolCall{Namespace: "tenant", FunctionCallMessage: &responses.FunctionCallMessage{Arguments: tc.foreign}}
			if _, err := tc.tool.Execute(context.Background(), call); err == nil || !strings.Contains(err.Error(), "unknown field") {
				t.Fatal("accepted arguments from another tool", err)
			}
			for _, body := range []string{"null", "{} {}"} {
				call.Arguments = body
				if _, err := tc.tool.Execute(context.Background(), call); err == nil || !strings.Contains(err.Error(), "expected one JSON object") {
					t.Fatal(body, err)
				}
			}
		})
	}
}

func TestIndividualRoutineManagementTools(t *testing.T) {
	ctx := context.Background()
	svc := NewService(testStore(t), &fakeExecutor{})
	factory := Tools(svc)
	r, err := svc.Create(ctx, "tenant", definition())
	if err != nil {
		t.Fatal(err)
	}
	d := definition()
	d.Name = "Updated routine"
	body, _ := json.Marshal(d)
	call := &agents.ToolCall{Namespace: "tenant", FunctionCallMessage: &responses.FunctionCallMessage{Arguments: fmt.Sprintf(`{"id":%q,"routine":%s}`, r.ID, body)}}
	if _, err := factory.UpdateRoutineTool().Execute(ctx, call); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(ctx, "tenant", r.ID)
	if err != nil || got.Name != d.Name {
		t.Fatal(got, err)
	}
	call.Arguments = fmt.Sprintf(`{"id":%q}`, r.ID)
	if _, err := factory.PauseRoutineTool().Execute(ctx, call); err != nil {
		t.Fatal(err)
	}
	got, _ = svc.Get(ctx, "tenant", r.ID)
	if got.Enabled {
		t.Fatal("pause failed")
	}
	if _, err := factory.ResumeRoutineTool().Execute(ctx, call); err != nil {
		t.Fatal(err)
	}
	got, _ = svc.Get(ctx, "tenant", r.ID)
	if !got.Enabled {
		t.Fatal("resume failed")
	}
	call.Arguments = "{}"
	result, err := factory.ListRoutinesTool().Execute(ctx, call)
	if err != nil {
		t.Fatal(err)
	}
	var rows []Routine
	if err := json.Unmarshal([]byte(*result.Output.OfString), &rows); err != nil || len(rows) != 1 || rows[0].Name != d.Name {
		t.Fatal(rows, err)
	}
	call.Namespace = "other"
	result, err = factory.ListRoutinesTool().Execute(ctx, call)
	if err != nil || *result.Output.OfString != "[]" {
		t.Fatal(result, err)
	}
	result, err = factory.ListAgentsTool().Execute(ctx, call)
	if err != nil || *result.Output.OfString != `["assistant"]` {
		t.Fatal(result, err)
	}
}
