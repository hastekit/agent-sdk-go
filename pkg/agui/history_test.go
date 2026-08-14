package agui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hastekit/hastekit-sdk-go/pkg/agents"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/history"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/messages"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/hastekit-sdk-go/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHistoryToMessages(t *testing.T) {
	rows := []history.ConversationMessage{{
		RunID:    "turn-1",
		ThreadID: "thread-1",
		Messages: []history.Message{
			messages.New("user", []responses.InputMessageUnion{
				responses.UserMessage("what's the weather?"),
			}),
			messages.New("Assistant", []responses.InputMessageUnion{
				{OfFunctionCall: &responses.FunctionCallMessage{
					ID: "fc_1", CallID: "call_1", Name: "get_weather", Arguments: `{"city":"Paris"}`,
				}},
				{OfFunctionCallOutput: &responses.FunctionCallOutputMessage{
					ID: "fco_1", CallID: "call_1",
					Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr("22C")},
				}},
				{OfOutputMessage: &responses.OutputMessage{
					ID: "msg_1", Role: constants.RoleAssistant,
					Content: &responses.OutputContent{
						{OfOutputText: &responses.OutputTextContent{Text: "It's 22C in Paris."}},
					},
				}},
				// Interrupt resolutions are skipped.
				{OfFunctionCallInterruptResolution: &responses.FunctionCallInterruptResolutionMessage{
					ID: "fcir_1", Resolutions: []responses.InterruptResolution{{CallID: "call_1", Action: "approve"}},
				}},
			}),
		},
	}}

	out := HistoryToMessages(rows)
	require.Len(t, out, 4)

	assert.Equal(t, RoleUser, out[0].Role)
	assert.Equal(t, "what's the weather?", out[0].Content)

	assert.Equal(t, RoleAssistant, out[1].Role)
	require.Len(t, out[1].ToolCalls, 1)
	assert.Equal(t, "call_1", out[1].ToolCalls[0].ID)
	assert.Equal(t, "get_weather", out[1].ToolCalls[0].Function.Name)

	assert.Equal(t, RoleTool, out[2].Role)
	assert.Equal(t, "call_1", out[2].ToolCallID)
	assert.Equal(t, "22C", out[2].Content)

	assert.Equal(t, RoleAssistant, out[3].Role)
	assert.Equal(t, "It's 22C in Paris.", out[3].Content)
}

func TestHistoryToMessagesCoalescesToolCalls(t *testing.T) {
	rows := []history.ConversationMessage{{
		Messages: []history.Message{
			messages.New("agent", []responses.InputMessageUnion{
				{OfFunctionCall: &responses.FunctionCallMessage{ID: "fc_1", CallID: "c1", Name: "a", Arguments: "{}"}},
				{OfFunctionCall: &responses.FunctionCallMessage{ID: "fc_2", CallID: "c2", Name: "b", Arguments: "{}"}},
			}),
		},
	}}

	out := HistoryToMessages(rows)
	require.Len(t, out, 1)
	assert.Len(t, out[0].ToolCalls, 2)
}

// File persistence round-trips an assistant OutputMessage into the
// EasyInput arm, with its text under output_text content. The
// conversion must still surface it as an assistant message rather than
// dropping it for "empty" text.
func TestHistoryToMessagesAssistantViaEasyInputOutputText(t *testing.T) {
	rows := []history.ConversationMessage{{
		Messages: []history.Message{
			messages.New("agent", []responses.InputMessageUnion{
				{OfEasyInput: &responses.EasyMessage{
					ID:   "msg_a",
					Role: constants.RoleAssistant,
					Content: responses.EasyInputContentUnion{
						OfInputMessageList: responses.InputContent{
							{OfOutputText: &responses.OutputTextContent{Text: "the answer is 42"}},
						},
					},
				}},
			}),
		},
	}}

	out := HistoryToMessages(rows)
	require.Len(t, out, 1)
	assert.Equal(t, RoleAssistant, out[0].Role)
	assert.Equal(t, "the answer is 42", out[0].Content)
}

// The SDK can store a function_call and its function_call_output under
// the same id. Emitting both verbatim collides in CopilotKit's
// id-keyed message list and drops the tool render; ids must be unique.
func TestHistoryToMessagesGivesUniqueIDs(t *testing.T) {
	rows := []history.ConversationMessage{{
		Messages: []history.Message{
			messages.New("agent", []responses.InputMessageUnion{
				{OfFunctionCall: &responses.FunctionCallMessage{ID: "dup", CallID: "c1", Name: "search", Arguments: "{}"}},
				{OfFunctionCallOutput: &responses.FunctionCallOutputMessage{ID: "dup", CallID: "c1",
					Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr("result")}}},
			}),
		},
	}}

	out := HistoryToMessages(rows)
	require.Len(t, out, 2)
	assert.NotEqual(t, out[0].ID, out[1].ID, "tool call and its result must not share an id")
	// The tool result still pairs to the call via toolCallId, untouched.
	assert.Equal(t, "c1", out[1].ToolCallID)
	assert.Equal(t, "c1", out[0].ToolCalls[0].ID)
}

func TestThreadsEndpoints(t *testing.T) {
	llm := &scriptedLLM{steps: []scriptedStep{{
		chunks: []*responses.ResponseChunk{
			messageAdded("msg_1"),
			textDelta("msg_1", "Hello there!"),
			messageDone("msg_1"),
		},
		response: assistantTextResponse("Hello there!"),
	}}}
	agent := agents.NewAgent(&agents.AgentOptions{Name: "Helper"}).WithLLM(llm)
	server := httptest.NewServer(NewHandler(registry{"Helper": agent}))
	defer server.Close()

	// Run one turn so a thread exists.
	postRun(t, server, "Helper", RunAgentInput{
		ThreadID: "thread-1",
		Messages: []Message{{ID: "u1", Role: RoleUser, Content: "say hello"}},
	})

	// List threads.
	res, err := http.Get(server.URL + "/agents/Helper/threads")
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)

	var listing struct {
		Threads []history.ThreadInfo `json:"threads"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&listing))
	require.Len(t, listing.Threads, 1)
	assert.Equal(t, "thread-1", listing.Threads[0].ThreadID)
	assert.Equal(t, "say hello", listing.Threads[0].Title)

	// Fetch the thread's history as AG-UI messages.
	res, err = http.Get(server.URL + "/agents/Helper/threads/thread-1/messages")
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)

	var hydrate struct {
		ThreadID string    `json:"threadId"`
		Messages []Message `json:"messages"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&hydrate))
	assert.Equal(t, "thread-1", hydrate.ThreadID)
	require.Len(t, hydrate.Messages, 2)
	assert.Equal(t, RoleUser, hydrate.Messages[0].Role)
	assert.Equal(t, "say hello", hydrate.Messages[0].Content)
	assert.Equal(t, RoleAssistant, hydrate.Messages[1].Role)
	assert.Equal(t, "Hello there!", hydrate.Messages[1].Content)

	// Unknown agent → 404.
	res, err = http.Get(server.URL + "/agents/Nope/threads")
	require.NoError(t, err)
	defer res.Body.Close()
	assert.Equal(t, http.StatusNotFound, res.StatusCode)
}

func TestThreadsEndpointWithoutLister(t *testing.T) {
	// An adapter that satisfies ConversationPersistenceAdapter but not
	// ThreadLister → 501 so clients can hide the picker.
	agent := agents.NewAgent(&agents.AgentOptions{
		Name:    "Helper",
		History: history.NewConversationManager(noListPersistence{history.NewInMemoryConversationPersistence()}),
	}).WithLLM(&scriptedLLM{})
	server := httptest.NewServer(NewHandler(registry{"Helper": agent}))
	defer server.Close()

	res, err := http.Get(server.URL + "/agents/Helper/threads")
	require.NoError(t, err)
	defer res.Body.Close()
	assert.Equal(t, http.StatusNotImplemented, res.StatusCode)
	assert.True(t, strings.Contains(res.Header.Get("Content-Type"), "application/json"))
}

// noListPersistence wraps an adapter while hiding its ThreadLister
// implementation.
type noListPersistence struct {
	history.ConversationPersistenceAdapter
}

// Reasoning is persisted to history as an OfReasoning item carrying a
// summary; on reload it must rehydrate as a role="reasoning" message so
// the reasoning panel renders the same summary it showed live.
func TestHistoryToMessagesReasoning(t *testing.T) {
	rows := []history.ConversationMessage{{
		Messages: []history.Message{
			messages.New("agent", []responses.InputMessageUnion{
				{OfReasoning: &responses.ReasoningMessage{
					ID: "rs_1",
					Summary: []responses.SummaryTextContent{
						{Text: "First, check the weather API."},
						{Text: "Then format the result."},
					},
					EncryptedContent: utils.Ptr("enc-blob"),
				}},
				{OfOutputMessage: &responses.OutputMessage{
					ID: "msg_1", Role: constants.RoleAssistant,
					Content: &responses.OutputContent{
						{OfOutputText: &responses.OutputTextContent{Text: "It's sunny."}},
					},
				}},
			}),
		},
	}}

	out := HistoryToMessages(rows)
	require.Len(t, out, 2)

	assert.Equal(t, RoleReasoning, out[0].Role)
	assert.Equal(t, "First, check the weather API.\nThen format the result.", out[0].Content)
	assert.Equal(t, "enc-blob", out[0].EncryptedValue)

	assert.Equal(t, RoleAssistant, out[1].Role)
	assert.Equal(t, "It's sunny.", out[1].Content)
}

// An empty reasoning item (no summary, no encrypted content) carries
// nothing to render and is dropped, matching the live stream.
func TestHistoryToMessagesReasoningEmptySkipped(t *testing.T) {
	rows := []history.ConversationMessage{{
		Messages: []history.Message{
			messages.New("agent", []responses.InputMessageUnion{
				{OfReasoning: &responses.ReasoningMessage{ID: "rs_empty"}},
			}),
		},
	}}

	assert.Empty(t, HistoryToMessages(rows))
}

func TestHistoryToMessagesImageGeneration(t *testing.T) {
	rows := []history.ConversationMessage{{
		Messages: []history.Message{
			messages.New("agent", []responses.InputMessageUnion{
				{OfImageGenerationCall: &responses.ImageGenerationCallMessage{
					ID: "ig_1", OutputFormat: "png", Result: "BBBB",
				}},
			}),
		},
	}}

	out := HistoryToMessages(rows)
	require.Len(t, out, 1)
	assert.Equal(t, RoleAssistant, out[0].Role)
	assert.Equal(t, "![generated image](data:image/png;base64,BBBB)", out[0].Content)
}
