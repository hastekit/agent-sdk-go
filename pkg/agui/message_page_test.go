package agui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

func TestMessagePagination(t *testing.T) {
	ctx := context.Background()
	p := history.NewInMemoryConversationPersistence()
	previous := ""
	for i := 0; i < 55; i++ {
		id := fmt.Sprintf("run-%d", i)
		msg := responses.UserMessage(id)
		msg.OfInputMessage.ID = "msg-" + id
		var meta map[string]any
		if i == 54 {
			state := agentstate.NewRunState()
			state.BackgroundTasks = map[string]agentstate.BackgroundTask{"job": {TaskID: "job"}}
			meta = state.ToMeta()
		}
		require.NoError(t, p.SaveMessages(ctx, "default", "default", id, previous, "thread", "conversation", []history.Message{{Messages: []responses.InputMessageUnion{msg}}}, meta))
		previous = id
	}
	a := agents.NewAgent(&agents.AgentOptions{Name: "test", History: history.NewConversationManager(p)})
	h := NewHandler(registry{"test": a})
	get := func(query string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/agents/test/threads/thread/messages"+query, nil))
		return w
	}
	var page struct {
		Run        *ThreadRunState `json:"run"`
		Messages   []Message       `json:"messages"`
		NextCursor string          `json:"nextCursor"`
		HasMore    bool            `json:"hasMore"`
	}
	w := get("")
	require.Equal(t, 200, w.Code)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page))
	require.Len(t, page.Messages, 50)
	require.True(t, page.HasMore)
	require.Equal(t, "run-5", page.Messages[0].Content)
	w = get("?cursor=" + page.NextCursor)
	require.Equal(t, 200, w.Code)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page))
	require.NotNil(t, page.Run)
	require.Equal(t, "run-54", page.Run.RunID)
	require.Len(t, page.Run.BackgroundTasks, 1)
	require.Len(t, page.Messages, 5)
	require.False(t, page.HasMore)
	require.Empty(t, page.NextCursor)
	require.Equal(t, "run-0", page.Messages[0].Content)
	for _, query := range []string{"?limit=0", "?limit=201", "?limit=no", "?limit=1&limit=2", "?cursor=garbage", "?cursor=" + nextMessageCursor("other", "thread", "run-5"), "?cursor=" + nextMessageCursor("default", "other", "run-5"), "?cursor=" + nextMessageCursor("default", "thread", "missing")} {
		require.Equal(t, 400, get(query).Code, query)
	}
}

func TestHistoryMessageIDsAreStable(t *testing.T) {
	msg := responses.UserMessage("hello")
	rows := []history.ConversationMessage{{RunID: "run", Messages: []history.Message{{Messages: []responses.InputMessageUnion{msg, msg}}}}}
	first := HistoryToMessages(rows)
	require.Equal(t, first, HistoryToMessages(rows))
	require.NotEqual(t, first[0].ID, first[1].ID)
}
