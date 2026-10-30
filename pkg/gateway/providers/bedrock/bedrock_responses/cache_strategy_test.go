package bedrock_responses

import (
	"strings"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCacheStrategyMetaFieldIsStripped(t *testing.T) {
	in := &responses.Request{
		Model: "anthropic.claude-test",
		Input: responses.InputUnion{OfString: utils.Ptr("hi")},
		Parameters: responses.Parameters{
			ExtraFields: map[string]any{
				"cache_strategy": "default",
			},
		},
	}

	out := NativeRequestToConverseRequest(in)
	payload, err := sonic.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}

	body := string(payload)
	t.Logf("payload: %s", body)

	if strings.Contains(body, "cache_strategy") {
		t.Fatalf("payload contains cache_strategy: %s", body)
	}
	if !strings.Contains(body, `"cachePoint":{"type":"default"}`) {
		t.Fatalf("payload missing cachePoint with type=default: %s", body)
	}
	if strings.Contains(body, `"ttl"`) {
		t.Fatalf("payload should not contain ttl when cache_ttl is unset: %s", body)
	}
}

func TestCacheStrategyAlwaysEmitsDefaultType(t *testing.T) {
	// Even if the caller passes a non-"default" strategy value, we must still
	// emit type="default" — Bedrock rejects any other value.
	in := &responses.Request{
		Model: "anthropic.claude-test",
		Input: responses.InputUnion{OfString: utils.Ptr("hi")},
		Parameters: responses.Parameters{
			ExtraFields: map[string]any{
				"cache_strategy": "auto",
			},
		},
	}

	out := NativeRequestToConverseRequest(in)
	payload, _ := sonic.Marshal(out)
	body := string(payload)

	if !strings.Contains(body, `"cachePoint":{"type":"default"}`) {
		t.Fatalf("expected cachePoint type=default regardless of strategy value: %s", body)
	}
	if strings.Contains(body, `"type":"auto"`) {
		t.Fatalf("strategy value leaked into cachePoint type: %s", body)
	}
}

func TestCacheTTLPropagates(t *testing.T) {
	in := &responses.Request{
		Model: "anthropic.claude-test",
		Input: responses.InputUnion{OfString: utils.Ptr("hi")},
		Parameters: responses.Parameters{
			ExtraFields: map[string]any{
				"cache_strategy": "default",
				"cache_ttl":      "1h",
			},
		},
	}

	out := NativeRequestToConverseRequest(in)
	payload, _ := sonic.Marshal(out)
	body := string(payload)
	t.Logf("payload: %s", body)

	if !strings.Contains(body, `"cachePoint":{"type":"default","ttl":"1h"}`) {
		t.Fatalf("expected cachePoint with ttl=1h: %s", body)
	}
	if strings.Contains(body, "cache_ttl") {
		t.Fatalf("cache_ttl meta-field leaked into payload: %s", body)
	}
}

// The breakpoint's position is decided during the full request build, so these
// go through it rather than calling the helper, and a conversation is spelled
// out as the turns that produced it.
func cachedMessages(t *testing.T, msgs ...responses.InputMessageUnion) []ConverseMessage {
	t.Helper()
	out := NativeRequestToConverseRequest(&responses.Request{
		Model: "anthropic.claude-test",
		Input: responses.InputUnion{OfInputMessageList: msgs},
		Parameters: responses.Parameters{
			ExtraFields: map[string]any{"cache_strategy": "default"},
		},
	})
	return out.Messages
}

func userTurn(content ...responses.InputContentUnion) responses.InputMessageUnion {
	return responses.InputMessageUnion{OfInputMessage: &responses.InputMessage{
		Role: constants.RoleUser, Content: content,
	}}
}

func assistantTurn(text string) responses.InputMessageUnion {
	return responses.InputMessageUnion{OfEasyInput: &responses.EasyMessage{
		Role:    constants.RoleAssistant,
		Content: responses.EasyInputContentUnion{OfString: utils.Ptr(text)},
	}}
}

func textPart(text string) responses.InputContentUnion {
	return responses.InputContentUnion{OfInputText: &responses.InputTextContent{Text: text}}
}

func filePart(filename, data string) responses.InputContentUnion {
	return responses.InputContentUnion{OfInputFile: &responses.InputFileContent{
		FileName: utils.Ptr(filename), FileData: utils.Ptr(data),
	}}
}

const csvAttachment = "data:text/csv;base64,YSxiCg=="

// Appending here is what Bedrock rejects: "Cache point cannot be inserted
// after the preceding content block for this model".
func TestCachePointGoesBeforeATrailingDocument(t *testing.T) {
	msgs := cachedMessages(t, userTurn(textPart("what is in this?"), filePart("123.csv", csvAttachment)))

	require.Len(t, msgs, 1)
	require.Len(t, msgs[0].Content, 3)
	assert.NotNil(t, msgs[0].Content[0].Text)
	require.NotNil(t, msgs[0].Content[1].CachePoint, "the only legal position left")
	assert.NotNil(t, msgs[0].Content[2].Document)
}

// The breakpoint is derived per request and never stored, so the turn that
// carried the attachment does not keep it: the document falls behind the
// breakpoint as soon as the conversation moves on, and caches from there.
func TestCachePointMovesPastTheDocumentOnTheNextTurn(t *testing.T) {
	msgs := cachedMessages(t,
		userTurn(textPart("what is in this?"), filePart("123.csv", csvAttachment)),
		assistantTurn("two columns"),
		userTurn(textPart("and the second row?")),
	)

	require.Len(t, msgs, 3)
	for i, block := range msgs[0].Content {
		assert.Nil(t, block.CachePoint, "block %d of the turn that carried the document", i)
	}
	require.NotNil(t, msgs[0].Content[1].Document, "now inside the cached prefix")

	require.Len(t, msgs[2].Content, 2)
	assert.NotNil(t, msgs[2].Content[0].Text)
	assert.NotNil(t, msgs[2].Content[1].CachePoint, "the breakpoint walked forward")
}

// A tool loop reaches the same place by a different route, since Converse
// carries a tool result as a user message.
func TestCachePointFollowsAToolResultPastTheDocument(t *testing.T) {
	msgs := cachedMessages(t,
		userTurn(textPart("summarise it"), filePart("123.csv", csvAttachment)),
		responses.InputMessageUnion{OfFunctionCall: &responses.FunctionCallMessage{
			CallID: "call_1", Name: "read_rows", Arguments: "{}",
		}},
		responses.InputMessageUnion{OfFunctionCallOutput: &responses.FunctionCallOutputMessage{
			CallID: "call_1",
			Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr("12 rows")},
		}},
	)

	require.Len(t, msgs, 3)
	require.Len(t, msgs[2].Content, 2)
	assert.NotNil(t, msgs[2].Content[0].ToolResult)
	assert.NotNil(t, msgs[2].Content[1].CachePoint)
}

// Only non-PDF documents hit the conversion bug, and a PDF is the one kind of
// attachment worth caching on the turn it arrives.
func TestCachePointStaysAtTheEndForAPDF(t *testing.T) {
	msgs := cachedMessages(t, userTurn(
		textPart("summarise this"),
		filePart("report.pdf", "data:application/pdf;base64,JVBERi0="),
	))

	require.Len(t, msgs, 1)
	require.Len(t, msgs[0].Content, 3)
	assert.NotNil(t, msgs[0].Content[1].Document)
	assert.NotNil(t, msgs[0].Content[2].CachePoint, "a PDF needs no room made for it")
}

// Consecutive user messages merge into one content array, which is how a
// document ends up with content after it inside a single message. The
// adjacency is what Bedrock objects to, so the end is still free.
func TestCachePointStaysAtTheEndWhenTheDocumentIsNotLast(t *testing.T) {
	msgs := cachedMessages(t,
		userTurn(textPart("here"), filePart("123.csv", csvAttachment)),
		userTurn(textPart("summarise it")),
	)

	require.Len(t, msgs, 1, "the two user turns merge")
	require.Len(t, msgs[0].Content, 4)
	assert.NotNil(t, msgs[0].Content[1].Document)
	assert.NotNil(t, msgs[0].Content[2].Text)
	assert.NotNil(t, msgs[0].Content[3].CachePoint, "nothing illegal precedes it")
}

// Nowhere legal to put it. Dropping the message breakpoint costs a cache miss;
// sending it costs the turn. The tool list is cached either way.
func TestNoMessageCachePointWhenTheTurnIsOnlyADocument(t *testing.T) {
	out := NativeRequestToConverseRequest(&responses.Request{
		Model: "anthropic.claude-test",
		Input: responses.InputUnion{OfInputMessageList: []responses.InputMessageUnion{
			userTurn(filePart("123.csv", csvAttachment)),
		}},
		Tools: []responses.ToolUnion{{OfFunction: &responses.FunctionTool{
			Name:       "read_rows",
			Parameters: map[string]any{"type": "object"},
		}}},
		Parameters: responses.Parameters{
			ExtraFields: map[string]any{"cache_strategy": "default"},
		},
	})

	require.Len(t, out.Messages, 1)
	require.Len(t, out.Messages[0].Content, 1)
	assert.NotNil(t, out.Messages[0].Content[0].Document)

	require.NotNil(t, out.ToolConfig)
	require.Len(t, out.ToolConfig.Tools, 2)
	assert.NotNil(t, out.ToolConfig.Tools[1].CachePoint, "the tool list still caches")
}
