package restate_runtime

import (
	"context"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	restate "github.com/restatedev/sdk-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type bgTool struct{ *agents.BaseTool }

func newBgTool(name string) *bgTool {
	return &bgTool{BaseTool: &agents.BaseTool{
		ToolUnion: responses.ToolUnion{
			OfFunction: &responses.FunctionTool{
				Name:        name,
				Description: utils.Ptr("starts long work"),
				Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
			},
		},
	}}
}

func (t *bgTool) Execute(_ context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	return &agents.ToolCallResponse{TaskID: "task-1"}, nil
}

func (t *bgTool) AwaitTask(context.Context, agents.BackgroundTaskRef, agents.ProgressReporter) (agents.BackgroundResult, error) {
	return agents.BackgroundResult{Output: agents.BackgroundText("indexed 4210 documents")}, nil
}

type ordinaryTool struct{ *agents.BaseTool }

func (t *ordinaryTool) Execute(_ context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	return &agents.ToolCallResponse{}, nil
}

// The loop asks the wrapper, not the tool behind it, whether a task id is
// allowed. A wrapper that dropped the capability would fail every run that
// started one.
func TestRestateTool_KeepsTheBackgroundCapability(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()

	background := newRestateTool(nil, newBgTool("index"), broker)
	plain := newRestateTool(nil, &ordinaryTool{BaseTool: newBgTool("plain").BaseTool}, broker)

	_, isBackground := background.(agents.BackgroundTool)
	assert.True(t, isBackground, "a tool that starts tasks stays one through the wrapper")

	_, plainIsBackground := plain.(agents.BackgroundTool)
	assert.False(t, plainIsBackground, "an ordinary tool does not gain the capability")

	assert.Equal(t, "index", background.GetToolDescriptor().ToolUnion.OfFunction.Name,
		"the wrapper is a way of running the tool, not a different tool")
}

// Waiting happens in the service, on the real tool. Calling it on the wrapper
// would mean waiting inside the run that started the task, which is the one
// thing this whole path exists to avoid.
func TestRestateBackgroundTool_RefusesToWaitInTheRun(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()
	wrapper := newRestateTool(nil, newBgTool("index"), broker).(agents.BackgroundTool)

	_, err := wrapper.AwaitTask(context.Background(), agents.BackgroundTaskRef{}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "own invocation")
}

// --- the service finds the tool again on the far side of the send -----------

func backgroundService() *BackgroundTaskService {
	return NewBackgroundTaskService(map[string]*agents.AgentOptions{
		"main": {
			Name:  "main",
			Tools: []agents.Tool{newBgTool("index"), &ordinaryTool{BaseTool: newBgTool("plain").BaseTool}},
		},
		// Reached by handoff from main. It owns the tool; main owns the thread.
		"specialist": {
			Name:  "specialist",
			Tools: []agents.Tool{newBgTool("render")},
		},
	}, streambroker.NewMemoryStreamBroker())
}

func TestBackgroundTaskService_FindsTheTool(t *testing.T) {
	tool, err := backgroundService().backgroundTool(&BackgroundTaskInput{AgentName: "main", ToolName: "index"})

	require.NoError(t, err)
	require.NotNil(t, tool)

	result, err := tool.AwaitTask(context.Background(), agents.BackgroundTaskRef{}, nil)
	require.NoError(t, err)
	require.NotNil(t, result.Output)
	assert.Equal(t, "indexed 4210 documents", *result.Output.Output.OfString,
		"the service waits on the real tool, not on a wrapper of it")
}

func TestBackgroundTaskService_ReportsWhatItCannotFind(t *testing.T) {
	svc := backgroundService()

	cases := []struct {
		name  string
		in    *BackgroundTaskInput
		wants string
	}{
		{"unknown agent", &BackgroundTaskInput{AgentName: "other", ToolName: "index"}, "agent not found"},
		{"unknown tool", &BackgroundTaskInput{AgentName: "main", ToolName: "missing"}, "not found on agent"},
		{"tool cannot wait", &BackgroundTaskInput{AgentName: "main", ToolName: "plain"}, "does not wait for background tasks"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.backgroundTool(tc.in)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wants)
		})
	}
}

// The service has to bind under the name the runner sends to, or every task
// would be dispatched into nothing.
func TestBackgroundTaskService_BindsUnderTheNameTheRunnerSendsTo(t *testing.T) {
	definition := restate.Reflect(backgroundService())

	assert.Equal(t, BackgroundTaskServiceName, definition.Name())

	_, ok := definition.Handlers()["Await"]
	assert.True(t, ok, "the runner sends to Await")
}

func TestBackgroundToolName(t *testing.T) {
	assert.Equal(t, "index", backgroundToolName(newBgTool("index")))
	assert.Empty(t, backgroundToolName(&ordinaryTool{BaseTool: &agents.BaseTool{}}),
		"a tool with no function schema has no name to find it again by")
}

// After a handoff the two names differ: the run belongs to the agent it
// entered at, and the tool to the specialist that started the task. Looking
// for the tool on the owner would not find it.
func TestBackgroundTaskService_FindsTheToolOnTheAgentThatOwnsIt(t *testing.T) {
	tool, err := backgroundService().backgroundTool(&BackgroundTaskInput{
		AgentName:     "main",
		ToolAgentName: "specialist",
		ToolName:      "render",
	})

	require.NoError(t, err)
	require.NotNil(t, tool)
}

// Without a handoff there is only one agent, and older inputs carry no
// ToolAgentName at all.
func TestBackgroundTaskService_FallsBackToTheRunsAgent(t *testing.T) {
	tool, err := backgroundService().backgroundTool(&BackgroundTaskInput{
		AgentName: "main",
		ToolName:  "index",
	})

	require.NoError(t, err)
	require.NotNil(t, tool)
}
