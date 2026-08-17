package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/bytedance/sonic"
	"github.com/google/uuid"
	hastekit "github.com/hastekit/agent-sdk-go"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

// GetUserTool - runs immediately (no approval needed)
type GetUserTool struct {
	*agents.BaseTool
}

func NewGetUserTool() *GetUserTool {
	return &GetUserTool{
		BaseTool: &agents.BaseTool{
			RequiresApproval: false,
			ToolUnion: responses.ToolUnion{
				OfFunction: &responses.FunctionTool{
					Name:        "get_user",
					Description: utils.Ptr("Gets user information"),
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"user_id": map[string]any{"type": "string"},
						},
						"required": []string{"user_id"},
					},
				},
			},
		},
	}
}

func (t *GetUserTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	return &agents.ToolCallResponse{
		FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			ID:     params.ID,
			CallID: params.CallID,
			Output: responses.FunctionCallOutputContentUnion{
				OfString: utils.Ptr(`{"name": "John Doe", "email": "john@example.com"}`),
			},
		},
	}, nil
}

// DeleteUserTool - requires approval
type DeleteUserTool struct {
	*agents.BaseTool
}

func NewDeleteUserTool() *DeleteUserTool {
	return &DeleteUserTool{
		BaseTool: &agents.BaseTool{
			RequiresApproval: true, // Human approval required
			ToolUnion: responses.ToolUnion{
				OfFunction: &responses.FunctionTool{
					Name:        "delete_user",
					Description: utils.Ptr("Permanently deletes a user account"),
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"user_id": map[string]any{"type": "string"},
						},
						"required": []string{"user_id"},
					},
				},
			},
		},
	}
}

func (t *DeleteUserTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	args := map[string]any{}
	json.Unmarshal([]byte(params.Arguments), &args)

	return &agents.ToolCallResponse{
		FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			ID:     params.ID,
			CallID: params.CallID,
			Output: responses.FunctionCallOutputContentUnion{
				OfString: utils.Ptr(fmt.Sprintf("User %s has been deleted", args["user_id"])),
			},
		},
		StateUpdates: nil,
	}, nil
}

func main() {
	ctx := context.Background()

	client := hastekit.NewLLMClient([]hastekit.ProviderConfig{
		{
			ProviderName: hastekit.ProviderOpenAI,
			ApiKeys: []*hastekit.APIKeyConfig{
				{
					Name:   "Key 1",
					APIKey: os.Getenv("OPENAI_API_KEY"),
				},
			},
		},
	})

	agent := hastekit.NewAgent(&hastekit.AgentConfig{
		Name:        "User Manager",
		Instruction: hastekit.NewPrompt("You help manage user accounts."),
		LLM:         client.Model("OpenAI/gpt-4o-mini"),
		Tools: []agents.Tool{
			NewGetUserTool(),
			NewDeleteUserTool(),
		},
		History: hastekit.NewFileHistory("./conversations"),
	})

	threadID := uuid.New().String()

	// First execution - agent may request to delete a user.
	handle, err := agent.Execute(ctx, &agents.AgentInput{
		Namespace: "default",
		ThreadID:  threadID,
		Message: history.Message{
			Messages: []responses.InputMessageUnion{
				responses.UserMessage("Delete user 123"),
			},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	result, err := handle.Result()
	if err != nil {
		log.Fatal(err)
	}

	// If a tool needs human approval the run pauses with one Interrupt per
	// pending tool call. Resume by sending a FunctionCallInterruptResolution
	// on the same thread, approving or rejecting each call by ID.
	if result.Status == agentstate.RunStatusPaused {
		fmt.Println("Approval required for:", result.Interrupts)

		approvalResponse := responses.InputMessageUnion{
			OfFunctionCallInterruptResolution: &responses.FunctionCallInterruptResolutionMessage{
				ID: uuid.NewString(),
				Resolutions: []responses.InterruptResolution{
					{
						CallID: result.Interrupts[0].FunctionCallMessage.CallID,
						Action: responses.InterruptActionApprove,
					},
				},
			},
		}

		handle, err = agent.Execute(ctx, &agents.AgentInput{
			Namespace: "default",
			ThreadID:  threadID,
			Message: history.Message{
				Messages: []responses.InputMessageUnion{
					approvalResponse,
				},
			},
		})
		if err != nil {
			log.Fatal(err)
		}
		result, err = handle.Result()
		if err != nil {
			log.Fatal(err)
		}
	}

	buf, _ := sonic.Marshal(result.Output)
	fmt.Println("Final result:", string(buf))
}
