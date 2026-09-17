package workflow

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

func nodeCall(in *Input, id, name, args string) responses.FunctionCallMessage {
	callID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(in.RunID+"/"+id)).String()
	return responses.FunctionCallMessage{ID: callID, CallID: callID, Name: name, Arguments: args}
}
func metadataString(in *Input, key string) string { v, _ := in.Metadata[key].(string); return v }
func namespace(in *Input) string {
	v := metadataString(in, "namespace")
	if v == "" {
		return "default"
	}
	return v
}
func decodeValue(v any, out any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
func resumedMessages(in *Input, id string) ([]responses.InputMessageUnion, error) {
	decision, ok := in.Resume(id)
	if !ok {
		return nil, nil
	}
	var messages []responses.InputMessageUnion
	if err := decodeValue(decision["messages"], &messages); err != nil {
		return nil, err
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("node %q requires interrupt resolution messages", id)
	}
	return messages, nil
}

// NewMCPNode constructs a mcp node using a directly supplied dependency.
func NewMCPNode(id string, cfg MCPNodeConfig) (Node, error) {
	n, err := newBuiltinNode(id, "mcp")
	if err != nil {
		return nil, err
	}
	timeout, err := nodeTimeout(cfg.Timeout)
	if err != nil {
		return nil, err
	}

	server := cfg.Server
	cfg.Arguments, err = copyBindings(cfg.Arguments)
	if err != nil {
		return nil, err
	}
	if server == nil || cfg.Tool == "" {
		return nil, fmt.Errorf("a registered MCP server and tool are required")
	}
	n.execute = func(ctx context.Context, in *Input) (any, string, error) {
		var call agents.ToolCall
		if saved := in.Suspended[id]; saved != nil {
			if err := decodeValue(saved.Payload["call"], &call); err != nil {
				return nil, "", err
			}
			messages, err := resumedMessages(in, id)
			if err != nil {
				return nil, "", err
			}
			call.ShouldResume = true
			call.ResumeMessages = messages
		} else {
			args, err := resolveValue(ctx, cfg.Arguments, in, timeout)
			if err != nil {
				return nil, "", err
			}
			if args == nil {
				args = map[string]any{}
			}
			if _, ok := args.(map[string]any); !ok {
				return nil, "", fmt.Errorf("MCP arguments must be an object")
			}
			encoded, err := json.Marshal(args)
			if err != nil {
				return nil, "", err
			}
			message := nodeCall(in, id, cfg.Tool, string(encoded))
			cloned, err := integrationContext(in)
			if err != nil {
				return nil, "", err
			}
			call = agents.ToolCall{FunctionCallMessage: &message, Namespace: namespace(in), ThreadID: metadataString(in, "thread_id"), SessionID: metadataString(in, "session_id"), RunContext: cloned.(map[string]any), State: map[string]string{}}
		}
		if call.FunctionCallMessage == nil {
			return nil, "", fmt.Errorf("missing MCP continuation call")
		}
		listed, err := server.ListTools(ctx, call.RunContext)
		if err != nil {
			return nil, "", err
		}
		var tool agents.Tool
		for _, candidate := range listed {
			d := candidate.GetToolDescriptor()
			if d != nil && d.ToolUnion.OfFunction != nil && d.ToolUnion.OfFunction.Name == cfg.Tool {
				tool = candidate
				break
			}
		}
		if tool == nil {
			return nil, "", fmt.Errorf("MCP tool %q not found on %q", cfg.Tool, server.GetName())
		}
		// Respect connector-configured approval before making the actual call.
		if tool.GetToolDescriptor().RequiresApproval && !call.ShouldResume {
			return nil, "", Pause(map[string]any{"call": call, "approval": true, "interrupts": []responses.Interrupt{{FunctionCallMessage: *call.FunctionCallMessage, Mode: responses.InterruptModeApproval}}})
		}
		if saved := in.Suspended[id]; saved != nil && saved.Payload["approval"] == true {
			action, _ := in.Resumes[id]["action"].(string)
			if action == responses.InterruptActionReject {
				return map[string]any{"rejected": true}, DefaultPort, nil
			}
			if action != responses.InterruptActionApprove {
				return nil, "", fmt.Errorf("MCP approval requires approve or reject")
			}
			call.ShouldResume = false
			call.ResumeMessages = nil
		}
		result, err := tool.Execute(ctx, &call)
		if err != nil {
			return nil, "", err
		}
		if result == nil {
			return nil, "", fmt.Errorf("MCP tool returned nil")
		}
		if result.TaskID != "" {
			return nil, "", fmt.Errorf("background MCP tool results are not supported by workflow nodes")
		}
		if len(result.Interrupts) > 0 {
			if call.State == nil {
				call.State = map[string]string{}
			}
			for k, v := range result.StateUpdates {
				call.State[k] = v
			}
			return nil, "", Pause(map[string]any{"call": call, "interrupts": result.Interrupts})
		}
		return result.FunctionCallOutputMessage, DefaultPort, nil
	}
	return n, nil
}

// NewAgentNode constructs a agent node using a directly supplied dependency.
func NewAgentNode(id string, cfg AgentNodeConfig) (Node, error) {
	n, err := newBuiltinNode(id, "agent")
	if err != nil {
		return nil, err
	}
	timeout, err := nodeTimeout(cfg.Timeout)
	if err != nil {
		return nil, err
	}

	agent := cfg.Agent
	if _, err := copyBindings(cfg.Message); err != nil {
		return nil, err
	}
	if agent == nil || cfg.Message == "" {
		return nil, fmt.Errorf("a registered agent and message are required")
	}
	n.execute = func(ctx context.Context, in *Input) (any, string, error) {
		var request agents.AgentInput
		if saved := in.Suspended[id]; saved != nil {
			if err := decodeValue(saved.Payload["agent_input"], &request); err != nil {
				return nil, "", err
			}
			messages, err := resumedMessages(in, id)
			if err != nil {
				return nil, "", err
			}
			request.Message = history.Message{Messages: messages}
		} else {
			message, err := resolveValue(ctx, cfg.Message, in, timeout)
			if err != nil {
				return nil, "", err
			}
			text, ok := message.(string)
			if !ok {
				return nil, "", fmt.Errorf("agent message must be a string")
			}
			cloned, err := integrationContext(in)
			if err != nil {
				return nil, "", err
			}
			request = agents.AgentInput{Namespace: namespace(in), ThreadID: nodeCall(in, id, "", "").CallID, SessionID: metadataString(in, "session_id"), RunContext: cloned.(map[string]any), Message: history.Message{Messages: []responses.InputMessageUnion{{OfEasyInput: &responses.EasyMessage{Role: constants.RoleUser, Content: responses.EasyInputContentUnion{OfString: &text}}}}}}
		}
		result, err := agent.Run(ctx, &request)
		if err != nil {
			return nil, "", err
		}
		if result == nil {
			return nil, "", fmt.Errorf("agent returned nil")
		}
		if result.Status == agentstate.RunStatusPaused {
			if result.RunID == "" || len(result.Interrupts) == 0 {
				return nil, "", fmt.Errorf("paused agent returned no continuation or interrupts")
			}
			request.PreviousRunID = result.RunID
			request.RunID = ""
			return nil, "", Pause(map[string]any{"agent_input": request, "interrupts": result.Interrupts})
		}
		return result, DefaultPort, nil
	}
	return n, nil
}

// Preserve application run context for credential providers and nested agents.
func integrationContext(in *Input) (any, error) {
	if raw, exists := in.RunContext["context"]; exists {
		if parent, ok := raw.(map[string]any); ok && parent != nil {
			return jsonValue(parent)
		}
		return map[string]any{}, nil
	}
	return jsonValue(in.RunContext)
}
