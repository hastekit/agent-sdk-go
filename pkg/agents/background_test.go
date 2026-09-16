package agents_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// backgroundTool answers immediately with a task id and waits on a channel the
// test controls, so delivery is observed rather than raced.
type backgroundTool struct {
	*agents.BaseTool

	taskID  string
	release chan struct{}
	result  agents.BackgroundResult
	err     error

	// progress is emitted through the reporter before the result is returned.
	progress []agents.ToolProgress

	mu     sync.Mutex
	awaits []agents.BackgroundTaskRef
}

func newBackgroundTool(name, taskID string) *backgroundTool {
	return &backgroundTool{
		BaseTool: &agents.BaseTool{
			ToolUnion: responses.ToolUnion{
				OfFunction: &responses.FunctionTool{
					Name:        name,
					Description: utils.Ptr("starts long work"),
					Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
				},
			},
		},
		taskID:  taskID,
		release: make(chan struct{}),
		result:  agents.BackgroundResult{Output: agents.BackgroundText("indexed 4210 documents")},
	}
}

func (t *backgroundTool) Execute(_ context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	return &agents.ToolCallResponse{
		FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			ID:     params.ID,
			CallID: params.CallID,
			Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr("started; job " + t.taskID)},
		},
		TaskID: t.taskID,
	}, nil
}

func (t *backgroundTool) AwaitTask(ctx context.Context, task agents.BackgroundTaskRef, progress agents.ProgressReporter) (agents.BackgroundResult, error) {
	t.mu.Lock()
	t.awaits = append(t.awaits, task)
	t.mu.Unlock()

	<-t.release

	for _, p := range t.progress {
		progress.Report(ctx, p)
	}
	return t.result, t.err
}

func (t *backgroundTool) awaited() []agents.BackgroundTaskRef {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]agents.BackgroundTaskRef(nil), t.awaits...)
}

// A tool that answers with a task id but cannot be waited on.
type plainTaskTool struct{ *fakeTool }

func (t *plainTaskTool) Execute(_ context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	return &agents.ToolCallResponse{
		FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			ID:     params.ID,
			CallID: params.CallID,
			Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr("started")},
		},
		TaskID: "task-orphan",
	}, nil
}

// --- the run carries on ----------------------------------------------------

// The call is answered now; the run does not wait for the task.
func TestBackgroundTask_RunDoesNotWaitForTheTask(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "index", "{}"),
		textResponse("indexing started"),
		textResponse("all done"),
	}}
	tool := newBackgroundTool("index", "task-1")

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Tools: []agents.Tool{tool},
	}).WithLLM(llm)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-bg-1", Message: userMessage("index the docs"),
	})

	// The run finished while the task is still going — nothing has released it.
	requireStatus(t, out, agentstate.RunStatusCompleted)
	assert.Equal(t, 2, llm.callCount())

	close(tool.release)
	agent.WaitForBackgroundTasks()
}

func TestBackgroundTask_IsRecordedOnTheRunState(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "index", "{}"),
		textResponse("indexing started"),
		textResponse("all done"),
	}}
	tool := newBackgroundTool("index", "task-1")

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Tools: []agents.Tool{tool},
	}).WithLLM(llm)

	runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-bg-state", Message: userMessage("index the docs"),
	})

	refs := tool.awaited()
	require.Len(t, refs, 1, "the tool is asked to wait for the task it started")
	assert.Equal(t, "task-1", refs[0].TaskID)
	assert.Equal(t, "call_1", refs[0].CallID, "progress is keyed to the call the client already drew")
	assert.Equal(t, "index", refs[0].ToolName)
	assert.Equal(t, "thread-bg-state", refs[0].ThreadID)
	assert.Equal(t, agents.StreamIDForTask("test", "thread-bg-state", "task-1"), refs[0].TaskStreamID,
		"the task streams on a channel of its own, derivable from its id")
	assert.NotEmpty(t, refs[0].ThreadStreamID, "the result still goes to the thread")
	assert.NotEqual(t, refs[0].TaskStreamID, refs[0].ThreadStreamID)

	close(tool.release)
	agent.WaitForBackgroundTasks()
}

// --- delivery: the agent is idle -------------------------------------------

func TestBackgroundTask_WakesAnIdleAgent(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "index", "{}"),
		textResponse("indexing started"),
		textResponse("the index is ready"),
	}}
	tool := newBackgroundTool("index", "task-1")

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Tools: []agents.Tool{tool},
	}).WithLLM(llm)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-bg-idle", Message: userMessage("index the docs"),
	})
	requireStatus(t, out, agentstate.RunStatusCompleted)

	// The first run is over. Finishing the task now has to start a new one.
	close(tool.release)
	agent.WaitForBackgroundTasks()

	require.Equal(t, 3, llm.callCount(), "a finished task wakes the idle agent")

	woken := inputTexts(t, llm.request(2))
	require.GreaterOrEqual(t, len(woken), 2)

	// The framing and the result are separate content blocks: the result keeps
	// whatever shape the tool gave it, which is what lets it be an image.
	notice, output := woken[len(woken)-2], woken[len(woken)-1]
	assert.Contains(t, notice, "task-1")
	assert.Contains(t, notice, "index")
	assert.Equal(t, "indexed 4210 documents", output, "the result reaches the model as its own block")
}

// --- delivery: a run is still going ----------------------------------------

// waitTool blocks until every background task has been delivered, which puts
// the delivery inside the run rather than after it.
type waitTool struct {
	*agents.BaseTool
	agent func() *agents.Agent
}

func (t *waitTool) Execute(_ context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	t.agent().WaitForBackgroundTasks()
	return &agents.ToolCallResponse{
		FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			ID:     params.ID,
			CallID: params.CallID,
			Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr("waited")},
		},
	}, nil
}

func TestBackgroundTask_JoinsARunStillInFlight(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "index", "{}"),
		toolCallResponse("call_2", "hold", "{}"),
		textResponse("the index is ready"),
	}}
	tool := newBackgroundTool("index", "task-1")

	var agent *agents.Agent
	hold := &waitTool{
		BaseTool: &agents.BaseTool{
			ToolUnion: responses.ToolUnion{
				OfFunction: &responses.FunctionTool{
					Name:        "hold",
					Description: utils.Ptr("waits"),
					Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
				},
			},
		},
		agent: func() *agents.Agent { return agent },
	}

	agent = agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Tools: []agents.Tool{tool, hold},
	}).WithLLM(llm)

	// Release straight away: the hold tool is what keeps the run alive long
	// enough for the delivery to land inside it.
	close(tool.release)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-bg-live", Message: userMessage("index the docs"),
	})
	requireStatus(t, out, agentstate.RunStatusCompleted)

	require.Equal(t, 3, llm.callCount(), "the result folds into the live run, it does not start a second one")

	third := inputTexts(t, llm.request(2))
	joined := false
	for _, text := range third {
		if strings.Contains(text, "indexed 4210 documents") {
			joined = true
		}
	}
	assert.True(t, joined, "the run that started the task sees the result at its next iteration")
}

// --- progress ---------------------------------------------------------------

// Progress publishes to the task's own channel, so it survives the thread
// moving on — including a later run claiming the thread's channel, which
// resets that channel's transcript.
func TestBackgroundTask_ProgressSurvivesTheThreadMovingOn(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "index", "{}"),
		textResponse("indexing started"),
		textResponse("the index is ready"),
	}}
	tool := newBackgroundTool("index", "task-1")
	tool.progress = []agents.ToolProgress{
		{Progress: 40, Total: 100, Message: "reading"},
		{Progress: 90, Total: 100, Message: "writing"},
	}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Tools: []agents.Tool{tool},
	}).WithLLM(llm)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-bg-progress", Message: userMessage("index the docs"),
	})
	requireStatus(t, out, agentstate.RunStatusCompleted)

	// Finishing now reports progress and then wakes the agent, whose new run
	// claims the thread's channel and wipes its transcript.
	close(tool.release)
	agent.WaitForBackgroundTasks()
	require.Equal(t, 3, llm.callCount())

	// Subscribing after all of that still gets the whole account of the task.
	taskStream := agents.StreamIDForTask("test", "thread-bg-progress", "task-1")
	chunks, err := agent.StreamBroker().Subscribe(context.Background(), taskStream)
	require.NoError(t, err)

	var seen []*responses.ChunkToolProgress[constants.ChunkTypeToolProgress]
	for chunk := range chunks {
		if chunk.OfToolProgress != nil {
			seen = append(seen, chunk.OfToolProgress)
		}
	}

	require.Len(t, seen, 2, "the task's progress is not the thread's to reset")
	assert.Equal(t, "call_1", seen[0].CallID, "keyed to the call, not to a new one")
	assert.Equal(t, "index", seen[0].ToolName)
	assert.Equal(t, "reading", seen[0].Message)
	assert.Equal(t, "writing", seen[1].Message)
}

// A client watching a task sees the channel close when the task ends.
func TestBackgroundTask_ClosesItsOwnStream(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "index", "{}"),
		textResponse("indexing started"),
		textResponse("the index is ready"),
	}}
	tool := newBackgroundTool("index", "task-1")

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Tools: []agents.Tool{tool},
	}).WithLLM(llm)

	runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-bg-close", Message: userMessage("index the docs"),
	})

	taskStream := agents.StreamIDForTask("test", "thread-bg-close", "task-1")
	chunks, err := agent.StreamBroker().Subscribe(context.Background(), taskStream)
	require.NoError(t, err)

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for range chunks {
		}
	}()

	close(tool.release)
	agent.WaitForBackgroundTasks()

	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the task's stream was left open after the task ended")
	}
}

// --- failure ----------------------------------------------------------------

// A wait that breaks down is not a task that failed, and the model is told so.
func TestBackgroundTask_ReportsAWaitThatFailed(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "index", "{}"),
		textResponse("indexing started"),
		textResponse("sorry, that did not work"),
	}}
	tool := newBackgroundTool("index", "task-1")
	tool.err = errors.New("indexer unreachable")

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Tools: []agents.Tool{tool},
	}).WithLLM(llm)

	runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-bg-fail", Message: userMessage("index the docs"),
	})

	close(tool.release)
	agent.WaitForBackgroundTasks()

	require.Equal(t, 3, llm.callCount())
	woken := inputTexts(t, llm.request(2))
	assert.Contains(t, woken[len(woken)-1], "indexer unreachable",
		"a wait that failed carries no result block, only the reason")
}

// --- guards -----------------------------------------------------------------

func TestBackgroundTask_ToolThatCannotBeWaitedOnFailsTheRun(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "orphan", "{}"),
	}}
	tool := &plainTaskTool{fakeTool: newFakeTool("orphan", false, "started")}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Tools: []agents.Tool{tool},
	}).WithLLM(llm)

	handle, err := agent.Execute(context.Background(), &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-bg-orphan", Message: userMessage("go"),
	})
	require.NoError(t, err)

	_, err = handle.Result()
	require.Error(t, err, "the work has started and nothing could ever report it")
	assert.Contains(t, err.Error(), "BackgroundTool")
}

func TestBackgroundTask_UnsupportedUnderADurableRuntime(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "index", "{}"),
	}}
	tool := newBackgroundTool("index", "task-1")

	// A DurableStep is what a durable runtime installs; the wait would end
	// with the activity that opened it, so there is nothing to wait with.
	agent := agents.NewAgent(&agents.AgentOptions{
		Name:        "main",
		Tools:       []agents.Tool{tool},
		DurableStep: passthroughStep{},
	}).WithLLM(llm)

	handle, err := agent.Execute(context.Background(), &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-bg-durable", Message: userMessage("go"),
	})
	require.NoError(t, err)

	_, err = handle.Result()
	require.Error(t, err)
	assert.ErrorIs(t, err, agents.ErrBackgroundUnsupported)
}

type passthroughStep struct{}

func (passthroughStep) Do(fn func()) { fn() }

// --- state helpers ----------------------------------------------------------

func TestRunStateBackgroundTasks(t *testing.T) {
	state := agentstate.NewRunState()
	assert.False(t, state.HasBackgroundTasks())

	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	state.AddBackgroundTask(agentstate.BackgroundTask{TaskID: "b", StartedAt: base.Add(time.Second)})
	state.AddBackgroundTask(agentstate.BackgroundTask{TaskID: "a", StartedAt: base})
	state.AddBackgroundTask(agentstate.BackgroundTask{TaskID: "", StartedAt: base})

	require.True(t, state.HasBackgroundTasks())
	list := state.BackgroundTaskList()
	require.Len(t, list, 2, "a task with no id is not a task")
	assert.Equal(t, "a", list[0].TaskID, "oldest first, so every replay iterates alike")
	assert.Equal(t, "b", list[1].TaskID)
}

func TestRunStateBackgroundTasksSurviveMeta(t *testing.T) {
	state := agentstate.NewRunState()
	state.AddBackgroundTask(agentstate.BackgroundTask{
		TaskID: "task-1", CallID: "call_1", ToolName: "index",
		StartedAt: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC),
	})

	reloaded := agentstate.LoadRunStateFromMeta(state.ToMeta())

	require.NotNil(t, reloaded)
	require.True(t, reloaded.HasBackgroundTasks())
	assert.Equal(t, "index", reloaded.BackgroundTasks["task-1"].ToolName)
	assert.Equal(t, "call_1", reloaded.BackgroundTasks["task-1"].CallID)
}

// A task's result is not assumed to be text. An image or a file travels as the
// content blocks the tool built, straight through to the model.
func TestBackgroundTask_DeliversNonTextResults(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "render", "{}"),
		textResponse("rendering started"),
		textResponse("here is the chart"),
	}}

	tool := newBackgroundTool("render", "task-1")
	tool.result = agents.BackgroundResult{
		Output: &responses.FunctionCallOutputMessage{
			Output: responses.FunctionCallOutputContentUnion{
				OfList: responses.InputContent{
					{OfInputText: &responses.InputTextContent{Text: "chart rendered"}},
					{OfInputImage: &responses.InputImageContent{ImageURL: utils.Ptr("data:image/png;base64,iVBORw0KGgo=")}},
				},
			},
		},
	}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Tools: []agents.Tool{tool},
	}).WithLLM(llm)

	runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-bg-image", Message: userMessage("render the chart"),
	})

	close(tool.release)
	agent.WaitForBackgroundTasks()
	require.Equal(t, 3, llm.callCount())

	content := lastUserContent(t, llm.request(2))
	require.Len(t, content, 3, "the notice, then the result's own blocks")

	require.NotNil(t, content[0].OfInputText)
	assert.Contains(t, content[0].OfInputText.Text, "task-1")

	require.NotNil(t, content[1].OfInputText)
	assert.Equal(t, "chart rendered", content[1].OfInputText.Text)

	require.NotNil(t, content[2].OfInputImage, "the image survives the trip")
	assert.Equal(t, "data:image/png;base64,iVBORw0KGgo=", *content[2].OfInputImage.ImageURL)
}

// lastUserContent is the content blocks of the final input message.
func lastUserContent(t *testing.T, req *responses.Request) responses.InputContent {
	t.Helper()
	list := req.Input.OfInputMessageList
	for i := len(list) - 1; i >= 0; i-- {
		if list[i].OfInputMessage != nil {
			return list[i].OfInputMessage.Content
		}
	}
	t.Fatal("no input message in the request")
	return nil
}

// --- handoffs ---------------------------------------------------------------

// handoffSetup is a root that hands off to a specialist which starts a
// background task. The specialist deliberately has no history of its own — the
// natural way to build one, since the root owns the conversation — which is
// what made a task delivered through the specialist open a conversation of its
// own.
func handoffSetup(t *testing.T, threadID string) (root *agents.Agent, rootLLM, specialistLLM *scriptedLLM, tool *backgroundTool) {
	t.Helper()

	broker := streambroker.NewMemoryStreamBroker()
	store := history.NewInMemoryConversationPersistence()

	specialistLLM = &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "index", "{}"),
		textResponse("indexing started"),
		textResponse("the index is ready"),
	}}
	tool = newBackgroundTool("index", "task-1")

	specialist := agents.NewAgent(&agents.AgentOptions{
		Name:         "specialist",
		Tools:        []agents.Tool{tool},
		StreamBroker: broker,
	}).WithLLM(specialistLLM)

	rootLLM = &scriptedLLM{script: []*responses.Response{
		toolCallResponse("h1", "transfer_to_agent", `{"agent_name":"specialist"}`),
		textResponse("the index is ready"),
	}}
	root = agents.NewAgent(&agents.AgentOptions{
		Name:         "root",
		History:      history.NewConversationManager(store),
		StreamBroker: broker,
		Handoffs:     []*agents.Handoff{agents.NewHandoff("specialist", "does indexing", specialist)},
	}).WithLLM(rootLLM)

	return root, rootLLM, specialistLLM, tool
}

// A specialist reached by handoff runs inside someone else's run. The task it
// starts belongs to that run's owner, so the result wakes the root — which can
// route back to the specialist itself — rather than the specialist directly.
func TestBackgroundTask_AfterHandoffWakesTheRunsOwner(t *testing.T) {
	root, rootLLM, specialistLLM, tool := handoffSetup(t, "thread-handoff")

	out := runAgent(t, root, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-handoff",
		StreamID: agents.StreamIDForThread("test", "thread-handoff"),
		Message:  userMessage("index the docs"),
	})
	requireStatus(t, out, agentstate.RunStatusCompleted)
	require.Equal(t, 1, rootLLM.callCount())
	require.Equal(t, 2, specialistLLM.callCount())

	close(tool.release)
	root.WaitForBackgroundTasks()

	assert.Equal(t, 2, rootLLM.callCount(), "the run enters at the agent that owns the thread")
	assert.Equal(t, 2, specialistLLM.callCount(), "not at the specialist that happened to start the task")
}

// The woken run has to be the same conversation, not a fresh one: the
// specialist has no history of its own, and delivering through it lost the
// thread entirely.
func TestBackgroundTask_AfterHandoffKeepsTheConversation(t *testing.T) {
	root, rootLLM, _, tool := handoffSetup(t, "thread-handoff-history")

	runAgent(t, root, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-handoff-history",
		StreamID: agents.StreamIDForThread("test", "thread-handoff-history"),
		Message:  userMessage("index the docs"),
	})

	close(tool.release)
	root.WaitForBackgroundTasks()

	require.Equal(t, 2, rootLLM.callCount())
	woken := inputTexts(t, rootLLM.request(1))
	assert.Contains(t, woken, "index the docs", "the thread the task belongs to, not a new one")
	assert.Contains(t, woken, "indexed 4210 documents")
}

// The task's own stream is keyed to the thread, which is the owner's, so a
// client watching it does not have to know a handoff happened.
func TestBackgroundTask_AfterHandoffStreamsOnTheThreadsChannels(t *testing.T) {
	root, _, _, tool := handoffSetup(t, "thread-handoff-stream")
	tool.progress = []agents.ToolProgress{{Progress: 1, Total: 1, Message: "working"}}

	runAgent(t, root, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-handoff-stream",
		StreamID: agents.StreamIDForThread("test", "thread-handoff-stream"),
		Message:  userMessage("index the docs"),
	})

	close(tool.release)
	root.WaitForBackgroundTasks()

	refs := tool.awaited()
	require.Len(t, refs, 1)
	assert.Equal(t, "root", refs[0].AgentName, "the run's owner")
	assert.Equal(t, agents.StreamIDForTask("test", "thread-handoff-stream", "task-1"), refs[0].TaskStreamID)

	chunks, err := root.StreamBroker().Subscribe(context.Background(), refs[0].TaskStreamID)
	require.NoError(t, err)

	var progress int
	for chunk := range chunks {
		if chunk.OfToolProgress != nil {
			progress++
		}
	}
	assert.Equal(t, 1, progress, "progress reaches the broker the thread streams on")
}

// --- events on the run's stream ---------------------------------------------

// chunksOf drains a handle, returning every chunk the run published.
func chunksOf(t *testing.T, handle *agents.AgentHandle) []*responses.ResponseChunk {
	t.Helper()
	var out []*responses.ResponseChunk
	for chunk := range handle.Chunks {
		out = append(out, chunk)
	}
	if _, err := handle.Wait(); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	return out
}

// Starting a task is announced on the run's own stream, with enough to follow
// it: which call it belongs to, and the stream it will publish progress on.
func TestBackgroundTask_AnnouncesTheStartOnTheRunsStream(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "index", "{}"),
		textResponse("indexing started"),
		textResponse("the index is ready"),
	}}
	tool := newBackgroundTool("index", "task-1")

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Tools: []agents.Tool{tool},
	}).WithLLM(llm)

	handle, err := agent.Execute(context.Background(), &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-bg-event",
		StreamID: agents.StreamIDForThread("test", "thread-bg-event"),
		Message:  userMessage("index the docs"),
	})
	require.NoError(t, err)

	var started *responses.ChunkBackgroundTask[constants.ChunkTypeBackgroundTaskStarted]
	for _, chunk := range chunksOf(t, handle) {
		if chunk.OfBackgroundTaskStarted != nil {
			started = chunk.OfBackgroundTaskStarted
		}
	}

	require.NotNil(t, started, "the run says a task is still working")
	assert.Equal(t, "task-1", started.TaskID)
	assert.Equal(t, "call_1", started.CallID, "which call it belongs to")
	assert.Equal(t, "index", started.ToolName)
	assert.Equal(t, agents.StreamIDForTask("test", "thread-bg-event", "task-1"), started.StreamID,
		"and where its progress will be published")

	close(tool.release)
	agent.WaitForBackgroundTasks()
}

// The result landing is announced too — on the run that took it in, which
// when the agent was idle is a run of its own.
func TestBackgroundTask_AnnouncesTheCompletionOnTheWokenRun(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "index", "{}"),
		textResponse("indexing started"),
		textResponse("the index is ready"),
	}}
	tool := newBackgroundTool("index", "task-1")
	broker := streambroker.NewMemoryStreamBroker()

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:         "main",
		Tools:        []agents.Tool{tool},
		StreamBroker: broker,
	}).WithLLM(llm)

	streamID := agents.StreamIDForThread("test", "thread-bg-done")
	handle, err := agent.Execute(context.Background(), &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-bg-done", StreamID: streamID,
		Message: userMessage("index the docs"),
	})
	require.NoError(t, err)
	chunksOf(t, handle)

	// The first run is over; finishing the task wakes a new one on the same
	// channel, which is where the completion is announced.
	close(tool.release)
	agent.WaitForBackgroundTasks()

	// Subscribed after the fact on purpose: claiming the channel wipes the
	// previous run's transcript, so what replays now is the woken run's own.
	rejoin, err := broker.Subscribe(context.Background(), streamID)
	require.NoError(t, err)

	var completed *responses.ChunkBackgroundTask[constants.ChunkTypeBackgroundTaskCompleted]
	for chunk := range rejoin {
		if chunk.OfBackgroundTaskCompleted != nil {
			completed = chunk.OfBackgroundTaskCompleted
		}
	}

	require.NotNil(t, completed, "the run that took the result in says so")
	assert.Equal(t, "task-1", completed.TaskID)
	assert.Equal(t, "call_1", completed.CallID,
		"announced where the task is known, so the call it belongs to comes with it")
	assert.Equal(t, "index", completed.ToolName)
	assert.Equal(t, agents.StreamIDForTask("test", "thread-bg-done", "task-1"), completed.StreamID)
}

// A task that lands while a run is still going folds into it, and the
// completion is announced on that run rather than waking a second one.
func TestBackgroundTask_AnnouncesTheCompletionOnALiveRun(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "index", "{}"),
		toolCallResponse("call_2", "hold", "{}"),
		textResponse("the index is ready"),
	}}
	tool := newBackgroundTool("index", "task-1")

	var agent *agents.Agent
	hold := &waitTool{
		BaseTool: &agents.BaseTool{
			ToolUnion: responses.ToolUnion{
				OfFunction: &responses.FunctionTool{
					Name:        "hold",
					Description: utils.Ptr("waits"),
					Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
				},
			},
		},
		agent: func() *agents.Agent { return agent },
	}

	agent = agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Tools: []agents.Tool{tool, hold},
	}).WithLLM(llm)

	close(tool.release)

	handle, err := agent.Execute(context.Background(), &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-bg-live-event",
		StreamID: agents.StreamIDForThread("test", "thread-bg-live-event"),
		Message:  userMessage("index the docs"),
	})
	require.NoError(t, err)

	var completed int
	for _, chunk := range chunksOf(t, handle) {
		if chunk.OfBackgroundTaskCompleted != nil {
			completed++
		}
	}

	assert.Equal(t, 1, completed, "announced on the run that folded it in, once")
	assert.Equal(t, 3, llm.callCount(), "and no second run was started")
}

// --- carrying across runs ---------------------------------------------------

// outstandingTasks reads what the thread is still waiting on, the way a page
// that has just loaded would.
func outstandingTasks(t *testing.T, agent *agents.Agent, namespace, threadID string) []agentstate.BackgroundTask {
	t.Helper()
	rows, err := agent.History().LoadTranscript(context.Background(), namespace, threadID)
	require.NoError(t, err)
	require.NotEmpty(t, rows)

	state := agentstate.LoadRunStateFromMeta(rows[len(rows)-1].Meta)
	require.NotNil(t, state)
	return state.BackgroundTaskList()
}

// A task outlives the run that started it, so a turn the user takes in the
// meantime must not lose track of it.
func TestBackgroundTask_CarriesAcrossAnUnrelatedTurn(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "index", "{}"),
		textResponse("indexing started"),
		textResponse("sure, here is something else"), // an unrelated turn
		textResponse("the index is ready"),
	}}
	tool := newBackgroundTool("index", "task-1")
	store := history.NewInMemoryConversationPersistence()

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:    "main",
		Tools:   []agents.Tool{tool},
		History: history.NewConversationManager(store),
	}).WithLLM(llm)

	in := func(text string) *agents.AgentInput {
		return &agents.AgentInput{
			Namespace: "test", ThreadID: "thread-carry",
			StreamID: agents.StreamIDForThread("test", "thread-carry"),
			Message:  userMessage(text),
		}
	}

	runAgent(t, agent, in("index the docs"))
	require.Len(t, outstandingTasks(t, agent, "test", "thread-carry"), 1,
		"the run that started it records it")

	// The user says something else entirely while the task is still working.
	runAgent(t, agent, in("what is the weather"))

	carried := outstandingTasks(t, agent, "test", "thread-carry")
	require.Len(t, carried, 1, "a turn of the user's own must not lose the task")
	assert.Equal(t, "task-1", carried[0].TaskID)
	assert.Equal(t, "index", carried[0].ToolName)

	close(tool.release)
	agent.WaitForBackgroundTasks()
}

// And it is dropped when its result arrives, or a thread that once started a
// task would report it as working forever.
func TestBackgroundTask_ClearedWhenTheResultArrives(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "index", "{}"),
		textResponse("indexing started"),
		textResponse("the index is ready"),
	}}
	tool := newBackgroundTool("index", "task-1")
	store := history.NewInMemoryConversationPersistence()

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:    "main",
		Tools:   []agents.Tool{tool},
		History: history.NewConversationManager(store),
	}).WithLLM(llm)

	runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-clear",
		StreamID: agents.StreamIDForThread("test", "thread-clear"),
		Message:  userMessage("index the docs"),
	})
	require.Len(t, outstandingTasks(t, agent, "test", "thread-clear"), 1)

	close(tool.release)
	agent.WaitForBackgroundTasks()

	assert.Empty(t, outstandingTasks(t, agent, "test", "thread-clear"),
		"the run woken by the result is not still waiting for it")
}

// The same, for a task that lands while a run is already going: it folds into
// that run, and that run is what drops it.
func TestBackgroundTask_ClearedWhenItFoldsIntoALiveRun(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "index", "{}"),
		toolCallResponse("call_2", "hold", "{}"),
		textResponse("the index is ready"),
	}}
	tool := newBackgroundTool("index", "task-1")
	store := history.NewInMemoryConversationPersistence()

	var agent *agents.Agent
	hold := &waitTool{
		BaseTool: &agents.BaseTool{
			ToolUnion: responses.ToolUnion{
				OfFunction: &responses.FunctionTool{
					Name:        "hold",
					Description: utils.Ptr("waits"),
					Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
				},
			},
		},
		agent: func() *agents.Agent { return agent },
	}

	agent = agents.NewAgent(&agents.AgentOptions{
		Name:    "main",
		Tools:   []agents.Tool{tool, hold},
		History: history.NewConversationManager(store),
	}).WithLLM(llm)

	close(tool.release)

	runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-clear-live",
		StreamID: agents.StreamIDForThread("test", "thread-clear-live"),
		Message:  userMessage("index the docs"),
	})

	assert.Empty(t, outstandingTasks(t, agent, "test", "thread-clear-live"),
		"the run it folded into is the one that drops it")
}
