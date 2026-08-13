package tools

import (
	"context"
	"fmt"

	"github.com/bytedance/sonic"
	"github.com/google/uuid"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/history"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/hastekit-sdk-go/pkg/utils"
)

type SubAgentContextMode string

const (
	SubAgentContextModeNone     SubAgentContextMode = "None"
	SubAgentContextModeIsolated SubAgentContextMode = "Isolated"
)

type AgentTool struct {
	*agents.BaseTool
	agent          *agents.Agent
	contextMode    SubAgentContextMode
	withoutTracing bool
}

type agentToolArgument struct {
	Message  string `json:"message"`
	ThreadID string `json:"thread_id"`
}

type AgentToolOption func(*AgentTool)

func WithoutTracing(t *AgentTool) {
	t.withoutTracing = true
}

func NewAgentTool(name string, description string, agent *agents.Agent, contextMode SubAgentContextMode, opts ...AgentToolOption) *AgentTool {
	toolUnion := responses.ToolUnion{
		OfFunction: &responses.FunctionTool{
			Name:        name,
			Description: utils.Ptr(description),
		},
	}

	switch contextMode {
	case SubAgentContextModeNone:
		toolUnion.OfFunction.Parameters = map[string]any{
			"type":     "object",
			"required": []string{"message"},
			"properties": map[string]any{
				"message": map[string]any{
					"type":        "string",
					"description": "Message for the agent",
				},
				"thread_id": map[string]any{
					"type":        "string",
					"description": "Thread ID for the agent conversation. Leave empty to start a new conversation.",
				},
			},
			"additionalProperties": false,
		}
	case SubAgentContextModeIsolated:
		toolUnion.OfFunction.Parameters = map[string]any{
			"type":     "object",
			"required": []string{"message"},
			"properties": map[string]any{
				"message": map[string]any{
					"type":        "string",
					"description": "Message for the agent",
				},
			},
			"additionalProperties": false,
		}
	}

	at := &AgentTool{
		BaseTool: &agents.BaseTool{
			ToolUnion: toolUnion,
		},
		agent:       agent,
		contextMode: contextMode,
	}

	for _, opt := range opts {
		opt(at)
	}

	return at
}

// Execute runs the inner agent. When params.ShouldResume is set,
// continues a previously paused call instead of starting a fresh
// one — recovers the inner thread id and prior AgentOutput from
// params.State (written on the earlier pause), then re-enters the
// inner agent with params.ResumeMessages (typically a single
// FunctionCallInterruptResolutionMessage). Otherwise, parses the LLM's
// arguments, picks/derives a thread id per contextMode, and starts
// the inner agent fresh.
//
// In both branches, the inner result is shaped by responseFromResult:
// a paused inner re-emits its Interrupts (and refreshes the saved
// state entries on params.State for the next resume); a completed
// inner produces the outer call's FunctionCallOutputMessage so the
// outer history regains its function_call ↔ function_call_output
// pair.
func (t *AgentTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	namespace := params.Namespace + "/" + params.Name

	var (
		threadId          string
		previousMessageID string
		messages          []responses.InputMessageUnion
	)

	if params.ShouldResume {
		if params.State == nil {
			return nil, fmt.Errorf("agent_tool: cannot resume — params.State missing")
		}

		runStateRaw, ok := params.State[t.getRunStateKey(params.ID)]
		if !ok || runStateRaw == "" {
			return nil, fmt.Errorf("agent_tool: cannot resume — saved run state missing for tool call %s", params.ID)
		}
		var savedResult agents.AgentOutput
		if err := sonic.Unmarshal([]byte(runStateRaw), &savedResult); err != nil {
			return nil, fmt.Errorf("agent_tool: malformed saved run state: %w", err)
		}
		if savedResult.RunID == "" {
			return nil, fmt.Errorf("agent_tool: saved run state has empty RunID for tool call %s", params.ID)
		}

		savedThreadId, ok := params.State[t.getResumeThreadIdStateKey(params.ID)]
		if !ok || savedThreadId == "" {
			return nil, fmt.Errorf("agent_tool: cannot resume — thread id missing for tool call %s", params.ID)
		}

		threadId = savedThreadId
		previousMessageID = savedResult.RunID
		messages = params.ResumeMessages
	} else {
		var agentArgs agentToolArgument
		if err := sonic.Unmarshal([]byte(params.Arguments), &agentArgs); err != nil {
			return nil, err
		}

		if t.contextMode == SubAgentContextModeIsolated {
			subAgentThreadId, exists := params.State[t.getSubAgentThreadIdStateKey()]
			if exists {
				threadId = subAgentThreadId
			} else {
				threadId = uuid.NewString()
			}
		} else {
			if agentArgs.ThreadID != "" {
				threadId = agentArgs.ThreadID
			} else {
				threadId = uuid.NewString()
			}
		}

		messages = []responses.InputMessageUnion{
			{
				OfEasyInput: &responses.EasyMessage{
					Role:    constants.RoleUser,
					Content: responses.EasyInputContentUnion{OfString: &agentArgs.Message},
				},
			},
		}
	}

	var result *agents.AgentOutput
	var err error
	agentInput := &agents.AgentInput{
		Namespace:         namespace,
		ThreadID:          threadId,
		PreviousMessageID: previousMessageID,
		Message:           history.Message{SenderID: params.AgentName, Messages: messages},
		SessionID:         params.SessionID, // Using conversation id as the shared session id
	}
	if t.withoutTracing {
		result, err = t.agent.ExecuteWithoutTrace(ctx, agentInput)
		if err != nil {
			return nil, err
		}
	} else {
		handle, err := t.agent.Execute(ctx, agentInput)
		if err != nil {
			return nil, err
		}

		// The parent agent reports tool output, not chunks. Result drains
		// the sub-agent's chunk stream and returns the aggregated output.
		result, err = handle.Result()
		if err != nil {
			return nil, err
		}
	}

	return t.responseFromResult(params, result, threadId)
}

// responseFromResult shapes an inner AgentOutput into the outer
// ToolCallResponse. Shared between Execute and Resume so the two
// paths stay in lockstep on shape, state-key naming, and the
// pause-vs-completion branch.
func (t *AgentTool) responseFromResult(params *agents.ToolCall, result *agents.AgentOutput, threadId string) (*agents.ToolCallResponse, error) {
	if result != nil && result.Status == agentstate.RunStatusPaused {
		resultBuf, err := sonic.Marshal(result)
		if err != nil {
			return nil, err
		}
		return &agents.ToolCallResponse{
			StateUpdates: map[string]string{
				t.getResumeThreadIdStateKey(params.ID): threadId,
				t.getRunStateKey(params.ID):            string(resultBuf),
			},
			Interrupts: result.Interrupts,
		}, nil
	}

	data := ""
	if result != nil {
		for _, out := range result.Output {
			if out.OfOutputMessage != nil {
				for _, content := range *out.OfOutputMessage.Content {
					if content.OfOutputText != nil {
						data += content.OfOutputText.Text
					}
				}
			}

			if out.OfEasyInput != nil {
				if out.OfEasyInput.Content.OfString != nil {
					data += *out.OfEasyInput.Content.OfString
				}

				if out.OfEasyInput.Content.OfInputMessageList != nil {
					for _, message := range out.OfEasyInput.Content.OfInputMessageList {
						if message.OfOutputText != nil {
							data += message.OfOutputText.Text
						}
					}
				}
			}
		}
	}

	if t.contextMode == SubAgentContextModeNone {
		data = data + fmt.Sprintf("\n---\nThread ID: %s", threadId)
	}

	return &agents.ToolCallResponse{
		FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			ID:     params.ID,
			CallID: params.CallID,
			Output: responses.FunctionCallOutputContentUnion{
				OfString: utils.Ptr(data),
			},
		},
		StateUpdates: map[string]string{
			t.getSubAgentThreadIdStateKey(): threadId,
		},
	}, nil
}

func (t *AgentTool) getSubAgentThreadIdStateKey() string {
	return fmt.Sprintf("sub_agent_thread_id/%s", t.agent.Name)
}

func (t *AgentTool) getResumeThreadIdStateKey(toolCallId string) string {
	return fmt.Sprintf("resume_thread_id/%s/%s", t.agent.Name, toolCallId)
}

func (t *AgentTool) getRunStateKey(toolCallId string) string {
	return fmt.Sprintf("run_state/%s/%s", t.agent.Name, toolCallId)
}
