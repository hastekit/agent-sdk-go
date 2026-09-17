package workflow

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// Tool exposes a workflow through the agent tool/interrupt protocol. Mutable
// state is serialized into ToolCallResponse.StateUpdates, never kept on Tool.
type Tool struct {
	*agents.BaseTool
	compiled   *Compiled
	options    []InvokeOption
	parameters *jsonschema.Resolved
}

// NewTool wraps either a YAML-loaded workflow or a programmatically compiled
// graph. Parameters describes the JSON object exposed as RunContext["input"].
func NewTool(name, description string, parameters map[string]any, compiled *Compiled, options ...InvokeOption) (*Tool, error) {
	if name == "" || compiled == nil {
		return nil, fmt.Errorf("workflow tool requires a name and compiled workflow")
	}
	if parameters == nil {
		parameters = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	var raw jsonschema.Schema
	if err := decodeValue(parameters, &raw); err != nil {
		return nil, fmt.Errorf("workflow tool schema: %w", err)
	}
	resolved, err := raw.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("workflow tool schema: %w", err)
	}
	return &Tool{BaseTool: &agents.BaseTool{ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{Name: name, Description: &description, Parameters: parameters}}}, compiled: compiled, parameters: resolved, options: append([]InvokeOption(nil), options...)}, nil
}
func (t *Tool) Execute(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
	if call == nil || call.FunctionCallMessage == nil || call.CallID == "" {
		return nil, fmt.Errorf("workflow tool requires a tool call ID")
	}
	key := "workflow:" + t.ToolUnion.OfFunction.Name + ":" + call.CallID
	var in Input
	if call.ShouldResume {
		if call.State[key] == "" {
			return nil, fmt.Errorf("workflow checkpoint missing for call %q", call.CallID)
		}
		if err := json.Unmarshal([]byte(call.State[key]), &in); err != nil {
			return nil, fmt.Errorf("workflow checkpoint: %w", err)
		}
		if in.Pause == nil {
			return nil, fmt.Errorf("workflow checkpoint is not paused")
		}
		interrupts, err := pauseInterrupts(in.Pause, &in)
		if err != nil {
			return nil, err
		}
		expected := map[string]bool{}
		for _, intr := range interrupts {
			expected[intr.FunctionCallMessage.CallID] = true
		}
		decision := map[string]any{}
		var resolutions []responses.InterruptResolution
		for _, message := range call.ResumeMessages {
			if message.OfFunctionCallInterruptResolution == nil {
				continue
			}
			for _, r := range message.OfFunctionCallInterruptResolution.Resolutions {
				if !expected[r.CallID] {
					continue
				}
				if r.Action != responses.InterruptActionApprove && r.Action != responses.InterruptActionReject {
					return nil, fmt.Errorf("invalid workflow resolution action")
				}
				decision["action"] = r.Action
				if len(r.Content) > 0 {
					var content any
					if err := json.Unmarshal(r.Content, &content); err != nil {
						return nil, err
					}
					decision["content"] = content
				}
				resolutions = append(resolutions, r)
			}
		}
		if len(resolutions) == 0 {
			return nil, fmt.Errorf("no resolution matches the paused workflow node")
		}
		decision["messages"] = []responses.InputMessageUnion{{OfFunctionCallInterruptResolution: &responses.FunctionCallInterruptResolutionMessage{Resolutions: resolutions}}}
		in.SetResume(in.Pause.NodeID, decision)
	} else {
		var args map[string]any
		if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
			return nil, fmt.Errorf("workflow arguments: %w", err)
		}
		if args == nil {
			return nil, fmt.Errorf("workflow arguments must be an object")
		}
		if err := t.parameters.Validate(args); err != nil {
			return nil, fmt.Errorf("workflow arguments: %w", err)
		}
		in = Input{RunID: uuid.NewString(), RunContext: map[string]any{"input": args, "context": call.RunContext}, Metadata: map[string]any{"namespace": call.Namespace, "thread_id": call.ThreadID, "session_id": call.SessionID}}
	}
	result, err := t.compiled.Execute(ctx, &in, t.options...)
	if err != nil {
		return nil, err
	}
	if result.Pause != nil {
		interrupts, err := pauseInterrupts(result.Pause, result)
		if err != nil {
			return nil, err
		}
		data, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		return &agents.ToolCallResponse{StateUpdates: map[string]string{key: string(data)}, Interrupts: interrupts}, nil
	}
	// Application run context may contain credentials; do not return it to the model.
	output := make(map[string]any, len(result.RunContext))
	for k, v := range result.RunContext {
		if k != "context" {
			output[k] = v
		}
	}
	data, err := json.Marshal(output)
	if err != nil {
		return nil, err
	}
	text := string(data)
	return &agents.ToolCallResponse{StateUpdates: map[string]string{key: ""}, FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{CallID: call.CallID, Output: responses.FunctionCallOutputContentUnion{OfString: &text}}}, nil
}
func pauseInterrupts(pause *PauseState, in *Input) ([]responses.Interrupt, error) {
	var interrupts []responses.Interrupt
	if raw, ok := pause.Payload["interrupts"]; ok {
		if err := decodeValue(raw, &interrupts); err != nil {
			return nil, err
		}
	}
	if len(interrupts) == 0 {
		data, err := json.Marshal(pause.Payload)
		if err != nil {
			return nil, err
		}
		interrupts = []responses.Interrupt{{FunctionCallMessage: nodeCall(in, pause.NodeID, "workflow_approval", string(data)), Mode: responses.InterruptModeApproval}}
	}
	return interrupts, nil
}

var _ agents.Tool = (*Tool)(nil)
