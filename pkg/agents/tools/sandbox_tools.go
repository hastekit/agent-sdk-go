package tools

import (
	"context"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

type sandboxHelper struct {
	sandboxManager sandbox.Manager
	image          string
	env            map[string]string
}

func (t *sandboxHelper) getClient(ctx context.Context, params *agents.ToolCall) (*sandbox.DaemonClient, string, error) {
	env := map[string]string{}
	for k, v := range t.env {
		env[k] = utils.TryAndParseAsTemplate(v, params.RunContext)
	}

	sb, err := t.sandboxManager.CreateSandbox(ctx, &sandbox.CreateSandboxRequest{
		SessionID: params.SessionID,
		Image:     t.image,
		AgentName: params.AgentName,
		Namespace: params.Namespace,
		Env:       env,
	})
	if err != nil {
		return nil, "", err
	}

	// Restore the working directory from state.
	workdir, ok := params.State[t.getSandboxCwdStateKey()]
	if !ok || workdir == "" {
		workdir = "/"
	}

	// Create a sandbox daemon client
	cli := sandbox.NewDaemonClient(sb)

	return cli, workdir, nil
}

func (t *sandboxHelper) getSandboxCwdStateKey() string {
	return "sandbox_cwd"
}

func (t *sandboxHelper) getStateUpdates(cwd string) map[string]string {
	// Propagate the new cwd back via state so later tool calls
	// (and durable workflow replays) start from the correct directory.
	var subAgentCtx map[string]string
	if cwd != "" {
		subAgentCtx = map[string]string{t.getSandboxCwdStateKey(): cwd}
	}

	return subAgentCtx
}

type BashTool struct {
	*agents.BaseTool
	sandboxHelper *sandboxHelper
}

type BashToolInput struct {
	Code string `json:"code"`
}

func NewBashTool(svc sandbox.Manager, image string, env map[string]string) *BashTool {
	return &BashTool{
		BaseTool: &agents.BaseTool{
			ToolUnion: responses.ToolUnion{
				OfFunction: &responses.FunctionTool{
					Name:        "execute_bash_commands",
					Description: utils.Ptr("Execute bash command and get the output"),
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
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
			sandboxManager: svc,
			image:          image,
			env:            env,
		},
	}
}

func (t *BashTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	var in BashToolInput
	err := sonic.Unmarshal([]byte(params.Arguments), &in)
	if err != nil {
		return nil, err
	}

	cli, workdir, err := t.sandboxHelper.getClient(ctx, params)
	if err != nil {
		return nil, err
	}

	// Run bash command
	res, err := cli.RunBashCommand(ctx, &sandbox.BashExecRequest{
		Command:        in.Code,
		Args:           nil,
		Script:         "",
		TimeoutSeconds: 0,
		Workdir:        workdir,
		Env:            nil,
	})
	if err != nil {
		return nil, err
	}

	// Serialize the output
	txt, _ := sonic.Marshal(res)

	return &agents.ToolCallResponse{
		FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			ID:     params.ID,
			CallID: params.CallID,
			Output: responses.FunctionCallOutputContentUnion{
				OfString: utils.Ptr(string(txt)),
			},
		},
		StateUpdates: t.sandboxHelper.getStateUpdates(res.Cwd),
	}, nil
}

type ReadFileTool struct {
	*agents.BaseTool
	sandboxHelper *sandboxHelper
}

type ReadFileToolInput struct {
	FilePath string `json:"file_path"`
}

func NewReadFileTool(svc sandbox.Manager, image string, env map[string]string) *ReadFileTool {
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
			sandboxManager: svc,
			image:          image,
			env:            env,
		},
	}
}

func (t *ReadFileTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	var in ReadFileToolInput
	err := sonic.Unmarshal([]byte(params.Arguments), &in)
	if err != nil {
		return nil, err
	}

	cli, _, err := t.sandboxHelper.getClient(ctx, params)
	if err != nil {
		return nil, err
	}

	// Run bash command
	res, err := cli.ReadFile(ctx, in.FilePath)
	if err != nil {
		return nil, err
	}

	// Serialize the output
	txt, _ := sonic.Marshal(res)

	return &agents.ToolCallResponse{
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

func NewDeleteFileTool(svc sandbox.Manager, image string, env map[string]string) *DeleteFileTool {
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
			sandboxManager: svc,
			image:          image,
			env:            env,
		},
	}
}

func (t *DeleteFileTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	var in ReadFileToolInput
	err := sonic.Unmarshal([]byte(params.Arguments), &in)
	if err != nil {
		return nil, err
	}

	cli, _, err := t.sandboxHelper.getClient(ctx, params)
	if err != nil {
		return nil, err
	}

	// Run bash command
	err = cli.DeleteFile(ctx, in.FilePath)
	if err != nil {
		return nil, err
	}

	return &agents.ToolCallResponse{
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

func NewWriteFileTool(svc sandbox.Manager, image string, env map[string]string) *WriteFileTool {
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
			sandboxManager: svc,
			image:          image,
			env:            env,
		},
	}
}

func (t *WriteFileTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	var in WriteFileToolInput
	err := sonic.Unmarshal([]byte(params.Arguments), &in)
	if err != nil {
		return nil, err
	}

	cli, _, err := t.sandboxHelper.getClient(ctx, params)
	if err != nil {
		return nil, err
	}

	// Run bash command
	resp, err := cli.WriteFile(ctx, in.FilePath, in.Content)
	if err != nil {
		return nil, err
	}

	return &agents.ToolCallResponse{
		FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			ID:     params.ID,
			CallID: params.CallID,
			Output: responses.FunctionCallOutputContentUnion{
				OfString: utils.Ptr("Written successfully to " + resp.Path),
			},
		},
	}, nil
}
