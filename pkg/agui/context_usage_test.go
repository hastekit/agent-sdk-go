package agui

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Each model call reports the context token count; a page that loads the
// thread later reads the same figure from the history endpoint.
func TestContextUsageIsStreamedAndStored(t *testing.T) {
	reply := assistantTextResponse("hi")
	reply.Usage = &responses.Usage{InputTokens: 1200, OutputTokens: 34, TotalTokens: 1234}
	agent := agents.NewAgent(&agents.AgentOptions{
		Name:    "Helper",
		History: history.NewConversationManager(history.NewInMemoryConversationPersistence()),
	}).WithLLM(&scriptedLLM{steps: []scriptedStep{{response: reply}}})

	server := httptest.NewServer(NewHandler(registry{"Helper": agent}))
	defer server.Close()

	frames := postRun(t, server, "Helper", RunAgentInput{
		ThreadID: "thread-context",
		Messages: []Message{{ID: "u1", Role: RoleUser, Content: "hi"}},
	})
	var usage []map[string]any
	for _, frame := range frames {
		if frame.data["name"] == CustomNameContextUsage {
			usage = append(usage, frame.data["value"].(map[string]any))
		}
	}
	require.Len(t, usage, 1, "one model call, one report")
	assert.Equal(t, map[string]any{"tokens": 1234.0, "agentName": "Helper"}, usage[0])

	loaded := getThreadMessagesWithContext(t, server, "Helper", "thread-context")
	require.NotNil(t, loaded.Context)
	assert.Equal(t, ContextUsage{Tokens: 1234, AgentName: "Helper"}, *loaded.Context)
}

// A provider that reports no usage gives the run nothing to stream; the
// stored figure is then the estimate of what the thread holds.
func TestContextUsageWithoutMeasurement(t *testing.T) {
	agent := agents.NewAgent(&agents.AgentOptions{
		Name:    "Helper",
		History: history.NewConversationManager(history.NewInMemoryConversationPersistence()),
	}).WithLLM(&scriptedLLM{steps: []scriptedStep{{response: assistantTextResponse("hi")}}})
	server := httptest.NewServer(NewHandler(registry{"Helper": agent}))
	defer server.Close()

	frames := postRun(t, server, "Helper", RunAgentInput{
		ThreadID: "thread-unmeasured",
		Messages: []Message{{ID: "u1", Role: RoleUser, Content: "hi"}},
	})
	for _, frame := range frames {
		assert.NotEqual(t, CustomNameContextUsage, frame.data["name"])
	}
	loaded := getThreadMessagesWithContext(t, server, "Helper", "thread-unmeasured")
	require.NotNil(t, loaded.Context)
	assert.Positive(t, loaded.Context.Tokens)
}

// The chunk crosses the Redis broker and durable boundaries as JSON.
func TestContextUsageChunkRoundTrips(t *testing.T) {
	chunk := &responses.ResponseChunk{OfContextUsage: &responses.ChunkContextUsage[constants.ChunkTypeContextUsage]{
		RunID: "run", AgentName: "Helper", Tokens: 42,
	}}
	data, err := sonic.Marshal(chunk)
	require.NoError(t, err)
	var decoded responses.ResponseChunk
	require.NoError(t, sonic.Unmarshal(data, &decoded))
	require.Equal(t, chunk.OfContextUsage, decoded.OfContextUsage)
	require.Equal(t, "context.usage", decoded.ChunkType())
}

type threadMessagesWithContext struct {
	Context *ContextUsage `json:"context"`
}

func getThreadMessagesWithContext(t *testing.T, server *httptest.Server, agentName, threadID string) threadMessagesWithContext {
	t.Helper()
	res, err := server.Client().Get(server.URL + "/agents/" + agentName + "/threads/" + threadID + "/messages")
	require.NoError(t, err)
	defer res.Body.Close()
	var out threadMessagesWithContext
	require.NoError(t, json.NewDecoder(res.Body).Decode(&out))
	return out
}
