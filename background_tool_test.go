package sdk_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	hastekit "github.com/hastekit/agent-sdk-go"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type indexArgs struct {
	Docs []string `json:"docs"`
}

type indexResult struct {
	Indexed int `json:"indexed"`
}

func indexTool(t *testing.T, release <-chan struct{}, fail error) (*hastekit.BackgroundFunctionTool[indexArgs, indexResult], *[]hastekit.ToolProgress, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	reported := &[]hastekit.ToolProgress{}

	tool := hastekit.NewBackgroundTool(
		func(ctx context.Context, in indexArgs, progress hastekit.ProgressReporter) (indexResult, error) {
			if release != nil {
				<-release
			}
			for i, doc := range in.Docs {
				update := hastekit.ToolProgress{Progress: float64(i + 1), Total: float64(len(in.Docs)), Message: doc}
				progress.Report(ctx, update)
				mu.Lock()
				*reported = append(*reported, update)
				mu.Unlock()
			}
			if fail != nil {
				return indexResult{}, fail
			}
			return indexResult{Indexed: len(in.Docs)}, nil
		},
		hastekit.WithName("index_docs"),
		hastekit.WithDescription("Index documents."),
	)
	return tool, reported, &mu
}

func call(args string) *agents.ToolCall {
	return &agents.ToolCall{
		FunctionCallMessage: &responses.FunctionCallMessage{
			ID: "fc_1", CallID: "call_1", Name: "index_docs", Arguments: args,
		},
	}
}

// --- descriptor -------------------------------------------------------------

func TestNewBackgroundTool_IsABackgroundTool(t *testing.T) {
	tool, _, _ := indexTool(t, nil, nil)

	var asTool agents.Tool = tool
	_, ok := asTool.(agents.BackgroundTool)
	assert.True(t, ok, "the loop only allows a task id from a tool it can wait on")
}

func TestNewBackgroundTool_DescribesItself(t *testing.T) {
	tool, _, _ := indexTool(t, nil, nil)

	descriptor := tool.GetToolDescriptor()
	require.NotNil(t, descriptor.ToolUnion.OfFunction)
	assert.Equal(t, "index_docs", descriptor.ToolUnion.OfFunction.Name)
	assert.Equal(t, "Index documents.", *descriptor.ToolUnion.OfFunction.Description)
	assert.NotNil(t, descriptor.ToolUnion.OfFunction.Parameters, "the schema comes from the argument type")
}

func TestNewBackgroundTool_TakesTheOrdinaryToolOptions(t *testing.T) {
	tool := hastekit.NewBackgroundTool(
		func(context.Context, indexArgs, hastekit.ProgressReporter) (indexResult, error) {
			return indexResult{}, nil
		},
		hastekit.WithName("index_docs"),
		hastekit.WithDestructive(true),
		hastekit.WithNeedsApproval(true),
	)

	descriptor := tool.GetToolDescriptor()
	assert.True(t, descriptor.RequiresApproval)
	assert.True(t, descriptor.Annotations.IsDestructive())
}

// --- starting ---------------------------------------------------------------

func TestNewBackgroundTool_AnswersImmediatelyWithATaskID(t *testing.T) {
	tool, _, _ := indexTool(t, nil, nil)

	resp, err := tool.Execute(context.Background(), call(`{"docs":["a","b"]}`))

	require.NoError(t, err)
	require.NotEmpty(t, resp.TaskID)
	assert.Contains(t, *resp.Output.OfString, resp.TaskID, "the model is told what to expect a result for")
	assert.JSONEq(t, `{"docs":["a","b"]}`, string(resp.TaskPayload),
		"the arguments travel with the task, since the wait may run elsewhere")
}

func TestNewBackgroundTool_RejectsArgumentsItCannotRead(t *testing.T) {
	tool, _, _ := indexTool(t, nil, nil)

	_, err := tool.Execute(context.Background(), call(`{"docs": "not a list"}`))

	require.Error(t, err, "the model can be told now; inside the wait there is nobody to tell")
}

func TestNewBackgroundTool_StartedMessageIsReplaceable(t *testing.T) {
	tool := hastekit.NewBackgroundTool(
		func(context.Context, indexArgs, hastekit.ProgressReporter) (indexResult, error) {
			return indexResult{}, nil
		},
		hastekit.WithName("index_docs"),
		hastekit.WithStartedMessage(func(taskID string) string { return "queued as " + taskID }),
	)

	resp, err := tool.Execute(context.Background(), call(`{"docs":[]}`))

	require.NoError(t, err)
	assert.Equal(t, "queued as "+resp.TaskID, *resp.Output.OfString)
}

// --- waiting ----------------------------------------------------------------

func TestNewBackgroundTool_RunsTheWorkWithTheArgumentsItStartedWith(t *testing.T) {
	tool, reported, mu := indexTool(t, nil, nil)

	resp, err := tool.Execute(context.Background(), call(`{"docs":["a","b","c"]}`))
	require.NoError(t, err)

	result, err := tool.AwaitTask(context.Background(), hastekit.BackgroundTaskRef{
		TaskID: resp.TaskID, Payload: resp.TaskPayload,
	}, nil)

	require.NoError(t, err)
	require.NotNil(t, result.Output)
	assert.JSONEq(t, `{"indexed":3}`, *result.Output.Output.OfString)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, *reported, 3, "progress is nil-safe: a tool reports whether or not anyone listens")
	assert.Equal(t, "c", (*reported)[2].Message)
}

func TestNewBackgroundTool_ReportsProgressToTheReporter(t *testing.T) {
	tool, _, _ := indexTool(t, nil, nil)

	resp, err := tool.Execute(context.Background(), call(`{"docs":["a","b"]}`))
	require.NoError(t, err)

	seen := &recordingReporter{}
	_, err = tool.AwaitTask(context.Background(), hastekit.BackgroundTaskRef{
		TaskID: resp.TaskID, Payload: resp.TaskPayload,
	}, seen)

	require.NoError(t, err)
	require.Len(t, seen.updates, 2)
	assert.Equal(t, float64(2), seen.updates[1].Total)
	assert.Equal(t, "b", seen.updates[1].Message)
}

func TestNewBackgroundTool_SurfacesTheWorksFailure(t *testing.T) {
	tool, _, _ := indexTool(t, nil, errors.New("indexer unreachable"))

	resp, err := tool.Execute(context.Background(), call(`{"docs":["a"]}`))
	require.NoError(t, err)

	_, err = tool.AwaitTask(context.Background(), hastekit.BackgroundTaskRef{
		TaskID: resp.TaskID, Payload: resp.TaskPayload,
	}, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "indexer unreachable")
}

type recordingReporter struct{ updates []hastekit.ToolProgress }

func (r *recordingReporter) Report(_ context.Context, u hastekit.ToolProgress) {
	r.updates = append(r.updates, u)
}

// --- end to end -------------------------------------------------------------

// scriptedLLM answers from a fixed script and records what it was asked.
type scriptedLLM struct {
	mu       sync.Mutex
	script   []*responses.Response
	requests []*responses.Request
}

func (s *scriptedLLM) NewStreamingResponses(_ context.Context, _ *agents.ModelCall, in *responses.Request, _ func(*responses.ResponseChunk)) (*responses.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, in)
	if len(s.requests) > len(s.script) {
		return nil, errors.New("scripted LLM exhausted")
	}
	return s.script[len(s.requests)-1], nil
}

func (s *scriptedLLM) inputTexts(i int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, msg := range s.requests[i].Input.OfInputMessageList {
		if msg.OfInputMessage == nil {
			continue
		}
		for _, c := range msg.OfInputMessage.Content {
			if c.OfInputText != nil {
				out = append(out, c.OfInputText.Text)
			}
		}
	}
	return out
}

func (s *scriptedLLM) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func textResponse(text string) *responses.Response {
	return &responses.Response{Output: []responses.OutputMessageUnion{{
		OfOutputMessage: &responses.OutputMessage{
			ID:      responses.NewOutputItemMessageID(),
			Role:    constants.RoleAssistant,
			Content: &responses.OutputContent{{OfOutputText: &responses.OutputTextContent{Text: text}}},
		},
	}}}
}

func toolCallResponse(callID, name, args string) *responses.Response {
	return &responses.Response{Output: []responses.OutputMessageUnion{{
		OfFunctionCall: &responses.FunctionCallMessage{
			ID: "fc_" + callID, CallID: callID, Name: name, Arguments: args,
		},
	}}}
}

// The whole point, exercised: the tool answers at once, the run finishes, and
// the result comes back on its own and wakes the agent.
func TestNewBackgroundTool_DeliversItsResultToTheAgent(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "index_docs", `{"docs":["a","b","c"]}`),
		textResponse("indexing started"),
		textResponse("the index is ready"),
	}}

	release := make(chan struct{})
	tool, _, _ := indexTool(t, release, nil)

	agent := hastekit.NewAgent(&hastekit.AgentConfig{
		Name:  "main",
		Tools: []hastekit.Tool{tool},
	}).WithLLM(llm)

	handle, err := agent.Execute(context.Background(), &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-helper", Message: history.Message{
			Messages: []responses.InputMessageUnion{responses.UserMessage("index the docs")},
		},
	})
	require.NoError(t, err)

	out, err := handle.Result()
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, 2, llm.calls(), "the run did not wait for the task")

	// The run is over. Finishing the work now has to wake the agent.
	close(release)
	agent.WaitForBackgroundTasks()

	require.Equal(t, 3, llm.calls(), "a finished task wakes the idle agent")
	woken := llm.inputTexts(2)
	require.NotEmpty(t, woken)
	assert.Contains(t, woken[len(woken)-1], `{"indexed":3}`, "the func's return value reaches the model")
}

// The work can answer with a tool output rather than a value to encode, which
// is how a background task returns an image or a file.
func TestNewBackgroundTool_PassesAToolOutputThrough(t *testing.T) {
	tool := hastekit.NewBackgroundTool(
		func(context.Context, indexArgs, hastekit.ProgressReporter) (*responses.FunctionCallOutputMessage, error) {
			return &responses.FunctionCallOutputMessage{
				Output: responses.FunctionCallOutputContentUnion{
					OfList: responses.InputContent{
						{OfInputImage: &responses.InputImageContent{ImageURL: utils.Ptr("data:image/png;base64,iVBORw0KGgo=")}},
					},
				},
			}, nil
		},
		hastekit.WithName("render"),
	)

	resp, err := tool.Execute(context.Background(), call(`{"docs":[]}`))
	require.NoError(t, err)

	result, err := tool.AwaitTask(context.Background(), hastekit.BackgroundTaskRef{
		TaskID: resp.TaskID, Payload: resp.TaskPayload,
	}, nil)

	require.NoError(t, err)
	require.NotNil(t, result.Output)
	require.Len(t, result.Output.Output.OfList, 1, "an output is carried, not encoded")
	require.NotNil(t, result.Output.Output.OfList[0].OfInputImage)
}
