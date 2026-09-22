package tools

import (
	"context"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

type sandboxHelper struct {
	provider sandbox.Provider
	profile  string
	env      map[string]string
}

func (t *sandboxHelper) getClient(ctx context.Context, params *agents.ToolCall) (sandbox.Sandbox, map[string]string, error) {
	env := map[string]string{}
	for k, v := range t.env {
		env[k] = utils.TryAndParseAsTemplate(v, params.RunContext)
	}
	return sandbox.Acquire(ctx, t.provider, params.State, sandbox.CreateRequest{Profile: t.profile, Env: env, AgentName: params.AgentName, Session: sandbox.SessionKey{Namespace: params.Namespace, SessionID: params.SessionID}})
}

type BashTool struct {
	*agents.BaseTool
	sandboxHelper *sandboxHelper
}

type BashToolInput struct {
	Code    string `json:"code"`
	Workdir string `json:"workdir,omitempty"`
}

func NewBashTool(svc sandbox.Provider, profile string, env map[string]string) *BashTool {
	return &BashTool{
		BaseTool: &agents.BaseTool{
			ToolUnion: responses.ToolUnion{
				OfFunction: &responses.FunctionTool{
					Name:        "execute_bash_commands",
					Description: utils.Ptr("Execute bash command and get the output"),
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"workdir": map[string]any{"type": "string", "description": "Working directory for this call. Defaults to the sandbox workspace. Directory changes do not persist between calls."},
							"code": map[string]any{
								"type":        "string",
								"description": "bash command to be executed",
							},
						},
						"required": []string{"code"},
					},
				},
			},
			RequiresApproval: false,
		},
		sandboxHelper: &sandboxHelper{
			provider: svc,
			profile:  profile,
			env:      env,
		},
	}
}

func (t *BashTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	var in BashToolInput
	err := sonic.Unmarshal([]byte(params.Arguments), &in)
	if err != nil {
		return nil, err
	}

	cli, updates, err := t.sandboxHelper.getClient(ctx, params)
	if err != nil {
		return nil, err
	}

	res, err := cli.Exec(ctx, sandbox.ExecRequest{Argv: []string{"bash", "-c", in.Code}, Workdir: in.Workdir})
	if err != nil {
		return sandboxToolError(ctx, params, updates, err)
	}

	// Serialize the output
	txt, _ := sonic.Marshal(res)

	return &agents.ToolCallResponse{
		StateUpdates: updates,
		FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			ID:     params.ID,
			CallID: params.CallID,
			Output: responses.FunctionCallOutputContentUnion{
				OfString: utils.Ptr(string(txt)),
			},
		},
	}, nil
}

type ReadFileTool struct {
	*agents.BaseTool
	sandboxHelper *sandboxHelper
}

type ReadFileToolInput struct {
	FilePath string `json:"file_path"`
}

// ReadFileToolOutput is the text tool response, separate from binary file transport.
type ReadFileToolOutput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func NewReadFileTool(svc sandbox.Provider, profile string, env map[string]string) *ReadFileTool {
	return &ReadFileTool{
		BaseTool: &agents.BaseTool{
			ToolUnion: responses.ToolUnion{
				OfFunction: &responses.FunctionTool{
					Name:        "read_file",
					Description: utils.Ptr("Read file content from the given file path"),
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"file_path": map[string]string{
								"type":        "string",
								"description": "File path to be read",
							},
						},
						"required": []string{"file_path"},
					},
				},
			},
		},
		sandboxHelper: &sandboxHelper{
			provider: svc,
			profile:  profile,
			env:      env,
		},
	}
}

func (t *ReadFileTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	var in ReadFileToolInput
	err := sonic.Unmarshal([]byte(params.Arguments), &in)
	if err != nil {
		return nil, err
	}

	cli, updates, err := t.sandboxHelper.getClient(ctx, params)
	if err != nil {
		return nil, err
	}

	// Read through the provider's native file API.
	reader, err := cli.Files().Read(ctx, in.FilePath)
	if err != nil {
		return sandboxToolError(ctx, params, updates, err)
	}

	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, (4<<20)+1))
	if err != nil {
		return sandboxToolError(ctx, params, updates, err)
	}
	if len(data) > 4<<20 {
		return sandboxToolError(ctx, params, updates, fmt.Errorf("file exceeds 4 MiB text tool limit; inspect it with shell commands"))
	}
	if !utf8.Valid(data) {
		return sandboxToolError(ctx, params, updates, fmt.Errorf("file is binary; inspect it with shell commands"))
	}
	res := ReadFileToolOutput{Path: in.FilePath, Content: string(data)}

	// Serialize the output
	txt, _ := sonic.Marshal(res)

	return &agents.ToolCallResponse{
		StateUpdates: updates,
		FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			ID:     params.ID,
			CallID: params.CallID,
			Output: responses.FunctionCallOutputContentUnion{
				OfString: utils.Ptr(string(txt)),
			},
		},
	}, nil
}

type DeleteFileTool struct {
	*agents.BaseTool
	sandboxHelper *sandboxHelper
}

type DeleteFileToolInput struct {
	FilePath string `json:"file_path"`
}

func NewDeleteFileTool(svc sandbox.Provider, profile string, env map[string]string) *DeleteFileTool {
	return &DeleteFileTool{
		BaseTool: &agents.BaseTool{
			ToolUnion: responses.ToolUnion{
				OfFunction: &responses.FunctionTool{
					Name:        "delete_file",
					Description: utils.Ptr("Delete file at the given file path"),
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"file_path": map[string]string{
								"type":        "string",
								"description": "File path to be deleted",
							},
						},
						"required": []string{"file_path"},
					},
				},
			},
		},
		sandboxHelper: &sandboxHelper{
			provider: svc,
			profile:  profile,
			env:      env,
		},
	}
}

func (t *DeleteFileTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	var in ReadFileToolInput
	err := sonic.Unmarshal([]byte(params.Arguments), &in)
	if err != nil {
		return nil, err
	}

	cli, updates, err := t.sandboxHelper.getClient(ctx, params)
	if err != nil {
		return nil, err
	}

	// Remove through the provider's native file API.
	err = cli.Files().Remove(ctx, in.FilePath)
	if err != nil {
		return sandboxToolError(ctx, params, updates, err)
	}

	return &agents.ToolCallResponse{
		StateUpdates: updates,
		FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			ID:     params.ID,
			CallID: params.CallID,
			Output: responses.FunctionCallOutputContentUnion{
				OfString: utils.Ptr("Deleted successfully"),
			},
		},
	}, nil
}

type WriteFileTool struct {
	*agents.BaseTool
	sandboxHelper *sandboxHelper
}

type WriteFileToolInput struct {
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

func NewWriteFileTool(svc sandbox.Provider, profile string, env map[string]string) *WriteFileTool {
	return &WriteFileTool{
		BaseTool: &agents.BaseTool{
			ToolUnion: responses.ToolUnion{
				OfFunction: &responses.FunctionTool{
					Name:        "write_file",
					Description: utils.Ptr("Write content to file at the given path"),
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"file_path": map[string]string{
								"type":        "string",
								"description": "File path to be written",
							},
							"content": map[string]string{
								"type":        "string",
								"description": "File content",
							},
						},
						"required": []string{"file_path", "content"},
					},
				},
			},
		},
		sandboxHelper: &sandboxHelper{
			provider: svc,
			profile:  profile,
			env:      env,
		},
	}
}

func (t *WriteFileTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	var in WriteFileToolInput
	err := sonic.Unmarshal([]byte(params.Arguments), &in)
	if err != nil {
		return nil, err
	}

	cli, updates, err := t.sandboxHelper.getClient(ctx, params)
	if err != nil {
		return nil, err
	}

	// Write through the provider's native file API.
	err = cli.Files().Write(ctx, in.FilePath, strings.NewReader(in.Content))
	if err != nil {
		return sandboxToolError(ctx, params, updates, err)
	}

	return &agents.ToolCallResponse{
		StateUpdates: updates,
		FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			ID:     params.ID,
			CallID: params.CallID,
			Output: responses.FunctionCallOutputContentUnion{
				OfString: utils.Ptr("Written successfully to " + in.FilePath),
			},
		},
	}, nil
}

// Operation failures are tool results so a successfully provisioned reference
// survives the failure and durable runtimes do not retry the whole creation.
func sandboxToolError(ctx context.Context, call *agents.ToolCall, updates map[string]string, err error) (*agents.ToolCallResponse, error) {
	if ctx.Err() != nil {
		return nil, err
	}
	result := agents.ToolCallResult(call, fmt.Sprintf("Tool execution failed: %v", err))
	result.StateUpdates = updates
	return result, nil
}
