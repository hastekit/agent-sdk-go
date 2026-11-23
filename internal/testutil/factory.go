package testutil

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

// EchoProvider is a deterministic model driven by directives in the system prompt:
// "call:<tool>" calls that tool once, "handoff:<agent>" transfers once, and otherwise
// it answers with the prompt plus the latest tool output.
type EchoProvider struct{ llm.Provider }

func (EchoProvider) NewStreamingResponses(_ context.Context, in *responses.Request) (chan *responses.ResponseChunk, error) {
	instructions := ""
	if in.Instructions != nil {
		instructions = *in.Instructions
	}
	offered := map[string]bool{}
	for _, tool := range in.Tools {
		if tool.OfFunction != nil {
			offered[tool.OfFunction.Name] = true
		}
	}
	called := map[string]bool{}
	lastOutput := ""
	for _, item := range in.Input.OfInputMessageList {
		if item.OfFunctionCall != nil {
			called[item.OfFunctionCall.Name] = true
		}
		if item.OfFunctionCallOutput != nil && item.OfFunctionCallOutput.Output.OfString != nil {
			lastOutput = *item.OfFunctionCallOutput.Output.OfString
		}
	}

	var output responses.OutputMessageUnion
	for _, word := range strings.Fields(instructions) {
		if target, ok := strings.CutPrefix(word, "handoff:"); ok && offered["transfer_to_agent"] && !called["transfer_to_agent"] {
			output = functionCall("transfer_to_agent", fmt.Sprintf(`{"agent_name":%q}`, target))
			break
		}
		if tool, ok := strings.CutPrefix(word, "call:"); ok && offered[tool] && !called[tool] {
			output = functionCall(tool, "{}")
			break
		}
	}
	if output.OfFunctionCall == nil {
		text := instructions
		if lastOutput != "" {
			text += " | " + lastOutput
		}
		output = responses.OutputMessageUnion{OfOutputMessage: &responses.OutputMessage{
			ID:      "msg_" + fmt.Sprint(len(in.Input.OfInputMessageList)),
			Role:    constants.RoleAssistant,
			Content: &responses.OutputContent{{OfOutputText: &responses.OutputTextContent{Text: text}}},
		}}
	}

	stream := make(chan *responses.ResponseChunk, 1)
	stream <- &responses.ResponseChunk{OfResponseCompleted: &responses.ChunkResponse[constants.ChunkTypeResponseCompleted]{
		Response: responses.ChunkResponseData{Output: []responses.OutputMessageUnion{output}},
	}}
	close(stream)
	return stream, nil
}

func functionCall(name, args string) responses.OutputMessageUnion {
	return responses.OutputMessageUnion{OfFunctionCall: &responses.FunctionCallMessage{
		ID: "fc_" + name, CallID: "call_" + name, Name: name, Arguments: args,
	}}
}

// FuncTool is a function tool whose behaviour is a closure.
type FuncTool struct {
	*agents.BaseTool
	Run func(ctx context.Context, call *agents.ToolCall) (string, error)
}

// NewFuncTool returns a tool named name that answers with run's text.
func NewFuncTool(name string, run func(ctx context.Context, call *agents.ToolCall) (string, error)) *FuncTool {
	return &FuncTool{
		BaseTool: &agents.BaseTool{ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{
			Name:        name,
			Description: utils.Ptr("test tool " + name),
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		}}},
		Run: run,
	}
}

// NewTextTool returns a tool that always answers text.
func NewTextTool(name, text string) *FuncTool {
	return NewFuncTool(name, func(context.Context, *agents.ToolCall) (string, error) { return text, nil })
}

// NewGatewayContextTool answers with the gateway context value under key, proving it reached the tool.
func NewGatewayContextTool(name, key string) *FuncTool {
	return NewFuncTool(name, func(ctx context.Context, _ *agents.ToolCall) (string, error) {
		return key + "=" + gateway.GetContext(ctx)[key], nil
	})
}

func (t *FuncTool) Execute(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
	text, err := t.Run(ctx, call)
	if err != nil {
		return nil, err
	}
	return &agents.ToolCallResponse{FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
		ID: call.ID, CallID: call.CallID, Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr(text)},
	}}, nil
}

// MapFactory is an AgentFactory over versioned builders, safe to change while serving.
type MapFactory struct {
	mu       sync.Mutex
	builders map[agents.AgentRef]func() *agents.AgentOptions
	builds   map[agents.AgentRef]int
}

func NewMapFactory() *MapFactory {
	return &MapFactory{builders: map[agents.AgentRef]func() *agents.AgentOptions{}, builds: map[agents.AgentRef]int{}}
}

// Put stores the builder for ref, replacing any earlier one.
func (f *MapFactory) Put(ref agents.AgentRef, build func() *agents.AgentOptions) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.builders[ref] = build
}

func (f *MapFactory) Build(_ context.Context, ref agents.AgentRef) (*agents.AgentOptions, error) {
	f.mu.Lock()
	build, ok := f.builders[ref]
	f.builds[ref]++
	f.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", agents.ErrAgentNotFound, ref)
	}
	return build(), nil
}

// Builds reports how often ref was built.
func (f *MapFactory) Builds(ref agents.AgentRef) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.builds[ref]
}

// BackgroundTool starts task TaskID when called and reports Result when awaited.
type BackgroundTool struct {
	*FuncTool
	TaskID string
	Result string
}

// NewBackgroundTool returns a tool that answers started now and result once its task is awaited.
func NewBackgroundTool(name, taskID, started, result string) *BackgroundTool {
	return &BackgroundTool{FuncTool: NewTextTool(name, started), TaskID: taskID, Result: result}
}

func (t *BackgroundTool) Execute(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
	resp, err := t.FuncTool.Execute(ctx, call)
	if err != nil {
		return nil, err
	}
	resp.TaskID = t.TaskID
	return resp, nil
}

func (t *BackgroundTool) AwaitTask(context.Context, agents.BackgroundTaskRef, agents.ProgressReporter) (agents.BackgroundResult, error) {
	return agents.BackgroundResult{Output: agents.BackgroundText(t.Result)}, nil
}
