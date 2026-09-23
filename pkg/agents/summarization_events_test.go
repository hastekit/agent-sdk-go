package agents_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/agui"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

type eventSummarizer struct {
	enabled       bool
	decisionError error
	run           func(context.Context, []history.Message) (*history.SummaryResult, error)
}

func (s eventSummarizer) ShouldSummarize(context.Context, map[string]string, []history.Message, int) (bool, error) {
	return s.enabled, s.decisionError
}

func (s eventSummarizer) Summarize(ctx context.Context, _ map[string]string, messages []history.Message, _ int) (*history.SummaryResult, error) {
	return s.run(ctx, messages)
}

func TestSummarizationEventsStreamAndReplay(t *testing.T) {
	for _, mode := range []string{"disabled", "compacted", "noop", "decision_error", "failed"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			broker := streambroker.NewMemoryStreamBroker()
			manager := history.NewConversationManager(history.NewInMemoryConversationPersistence())
			failure := errors.New("summarizer failed")
			if mode != "disabled" {
				var decisionError error
				if mode == "decision_error" {
					decisionError = failure
				}
				manager.Summarizer = eventSummarizer{decisionError: decisionError, enabled: mode != "noop", run: func(ctx context.Context, messages []history.Message) (*history.SummaryResult, error) {
					chunks, err := broker.Replay(ctx, "stream")
					require.NoError(t, err)
					require.NotEmpty(t, chunks)
					require.NotNil(t, chunks[len(chunks)-1].OfSummarizationStarted, "start must be visible while summarization is running")
					if mode == "failed" {
						return nil, failure
					}
					if mode == "noop" {
						return nil, nil
					}
					return &history.SummaryResult{MessagesToKeep: messages}, nil
				}}
			}
			llm := &scriptedLLM{script: []*responses.Response{textResponse("done")}}
			a := newScriptedAgent("test", llm, manager, broker, nil, nil)
			_, err := a.ExecuteLocal(ctx, &agents.AgentInput{ThreadID: "thread", StreamID: "stream", Message: userMessage("hello")})
			if mode == "failed" || mode == "decision_error" {
				require.ErrorIs(t, err, failure)
				require.Zero(t, llm.callCount())
			} else {
				require.NoError(t, err)
			}
			chunks, err := broker.Replay(ctx, "stream")
			require.NoError(t, err)
			var names []string
			translator := agui.NewTranslator("thread", "run")
			for _, chunk := range chunks {
				if chunk.OfSummarizationStarted == nil && chunk.OfSummarizationCompleted == nil {
					continue
				}
				// Redis and durable boundaries serialize these same wire chunks.
				data, err := sonic.Marshal(chunk)
				require.NoError(t, err)
				var replayed responses.ResponseChunk
				require.NoError(t, sonic.Unmarshal(data, &replayed))
				require.Equal(t, chunk.ChunkType(), replayed.ChunkType())
				events := translator.Translate(&replayed)
				require.Len(t, events, 1)
				event := events[0].(*agui.CustomEvent)
				names = append(names, event.Name)
				if end := replayed.OfSummarizationCompleted; end != nil {
					require.Equal(t, mode == "failed", end.Failed)
					require.Equal(t, mode == "compacted", end.Compacted)
					require.Equal(t, "test", end.AgentName)
					require.NotEmpty(t, end.RunID)
				}
			}
			if mode == "disabled" || mode == "noop" || mode == "decision_error" {
				require.Empty(t, names)
			} else {
				require.Equal(t, []string{agui.CustomNameSummarizationStarted, agui.CustomNameSummarizationCompleted}, names)
			}
		})
	}
}
