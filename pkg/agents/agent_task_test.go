package agents

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

func newTaskAgent(store history.ConversationPersistenceAdapter, llm *answeringLLM) *Agent {
	return NewAgent(&AgentOptions{
		Name:         "worker",
		History:      history.NewConversationManager(store),
		StreamBroker: streambroker.NewMemoryStreamBroker(),
	}).WithLLM(llm)
}

func taskInput(threadID, messageID, text string) *AgentInput {
	return &AgentInput{
		Namespace: "tenant",
		ThreadID:  threadID,
		StreamID:  StreamIDForThread("tenant", threadID),
		Message:   messages.NewWithID(messageID, "caller", []responses.InputMessageUnion{responses.UserMessage(text)}),
	}
}

func assistantReply(text string) responses.InputMessageUnion {
	return responses.InputMessageUnion{OfOutputMessage: &responses.OutputMessage{
		ID: "msg_" + text, Role: constants.RoleAssistant,
		Content: &responses.OutputContent{{OfOutputText: &responses.OutputTextContent{Text: text}}},
	}}
}

// A wait retried after its first attempt's run finished returns that run's
// answer, and does not run the turn again.
func TestRunAgentTaskRetriedAfterTheRunFinished(t *testing.T) {
	ctx := context.Background()
	store := history.NewInMemoryConversationPersistence()
	llm := &answeringLLM{text: "the answer"}
	agent := newTaskAgent(store, llm)

	first, err := RunAgentTask(ctx, agent, taskInput("copy", "task-1", "do it"))
	require.NoError(t, err)
	require.False(t, first.Joined)
	require.Equal(t, "the answer", first.Output.Text())
	require.Len(t, llm.requests, 1)

	retried, err := RunAgentTask(ctx, agent, taskInput("copy", "task-1", "do it"))
	require.NoError(t, err)
	require.Equal(t, "the answer", retried.Output.Text())
	require.Equal(t, agentstate.RunStatusCompleted, retried.Output.Status)
	require.Len(t, llm.requests, 1, "the turn was not run again")
	rows, err := history.LoadTranscript(ctx, store, "tenant", "copy")
	require.NoError(t, err)
	require.Len(t, rows, 1)
}

// A wait retried while its first attempt's run is still going waits for that
// run and returns its answer, instead of joining it as a stranger.
func TestRunAgentTaskRetriedWhileTheRunIsGoing(t *testing.T) {
	ctx := context.Background()
	prev := agentTaskPollInterval
	agentTaskPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { agentTaskPollInterval = prev })

	store := history.NewInMemoryConversationPersistence()
	llm := &answeringLLM{text: "unused"}
	agent := newTaskAgent(store, llm)

	// The first attempt's run has taken the message in and is working.
	state := agentstate.NewRunState()
	opening := messages.NewWithID("task-1", "caller", []responses.InputMessageUnion{responses.UserMessage("do it")})
	require.NoError(t, store.SaveMessages(ctx, "tenant", "", "", false, "run-1", "", "copy", "copy", []history.Message{opening}, state.ToMeta()))

	go func() {
		time.Sleep(50 * time.Millisecond)
		state.TransitionToComplete()
		_ = store.SaveMessages(ctx, "tenant", "", "", false, "run-1", "", "copy", "copy",
			[]history.Message{messages.New("worker", []responses.InputMessageUnion{assistantReply("finished meanwhile")})}, state.ToMeta())
	}()

	retried, err := RunAgentTask(ctx, agent, taskInput("copy", "task-1", "do it"))
	require.NoError(t, err)
	require.False(t, retried.Joined)
	require.Equal(t, "finished meanwhile", retried.Output.Text())
	require.Empty(t, llm.requests, "nothing was run again")
}

// A message that went into a run something else opened is reported as
// joined again; a run that failed is reported as an error.
func TestRunAgentTaskJoinedOrFailed(t *testing.T) {
	ctx := context.Background()
	store := history.NewInMemoryConversationPersistence()
	agent := newTaskAgent(store, &answeringLLM{text: "unused"})

	running := agentstate.NewRunState()
	require.NoError(t, store.SaveMessages(ctx, "tenant", "", "", false, "run-a", "", "busy", "busy", []history.Message{
		messages.NewWithID("other-task", "caller", []responses.InputMessageUnion{responses.UserMessage("first")}),
		messages.NewWithID("task-2", "caller", []responses.InputMessageUnion{responses.UserMessage("steered in")}),
	}, running.ToMeta()))
	joined, err := RunAgentTask(ctx, agent, taskInput("busy", "task-2", "steered in"))
	require.NoError(t, err)
	require.True(t, joined.Joined)

	failed := agentstate.NewRunState()
	failed.TransitionToError(errors.New("model down"))
	require.NoError(t, store.SaveMessages(ctx, "tenant", "", "", false, "run-b", "", "broken", "broken", []history.Message{
		messages.NewWithID("task-3", "caller", []responses.InputMessageUnion{responses.UserMessage("try")}),
	}, failed.ToMeta()))
	_, err = RunAgentTask(ctx, agent, taskInput("broken", "task-3", "try"))
	require.ErrorContains(t, err, "failed")
}
