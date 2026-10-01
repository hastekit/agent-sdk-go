package agui

import (
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Providers state a call's arguments in different places: some only stream
// them, some put a placeholder or the whole arguments on item_added and stream
// them too, some state them only at the end. However they do it, the client
// accumulates the arguments exactly once, as one JSON document, before the
// call ends.
func TestToolCallArgumentsReachTheClientOnce(t *testing.T) {
	const whole = `{"query":"quarterly revenue","limit":3}`
	added := func(args *string) *responses.ResponseChunk {
		return &responses.ResponseChunk{OfOutputItemAdded: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemAdded]{
			Item: responses.ChunkOutputItemData{Type: "function_call", Id: "item-1", CallID: utils.Ptr("call-1"), Name: utils.Ptr("lookup"), Arguments: args},
		}}
	}
	delta := func(d string) *responses.ResponseChunk {
		return &responses.ResponseChunk{OfFunctionCallArgumentsDelta: &responses.ChunkFunctionCall[constants.ChunkTypeFunctionCallArgumentsDelta]{ItemId: "item-1", Delta: d}}
	}
	argsDone := func(args string) *responses.ResponseChunk {
		return &responses.ResponseChunk{OfFunctionCallArgumentsDone: &responses.ChunkFunctionCall[constants.ChunkTypeFunctionCallArgumentsDone]{ItemId: "item-1", Arguments: args}}
	}
	itemDone := func(args *string) *responses.ResponseChunk {
		return &responses.ResponseChunk{OfOutputItemDone: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{
			Item: responses.ChunkOutputItemData{Type: "function_call", Id: "item-1", CallID: utils.Ptr("call-1"), Name: utils.Ptr("lookup"), Arguments: args},
		}}
	}

	for name, chunks := range map[string][]*responses.ResponseChunk{
		"streamed (OpenAI)":                           {added(utils.Ptr("")), delta(`{"query":`), delta(`"quarterly revenue",`), delta(`"limit":3}`), argsDone(whole), itemDone(utils.Ptr(whole))},
		"placeholder on item_added (Anthropic)":       {added(utils.Ptr("{}")), delta(`{"query":"quarterly revenue",`), delta(`"limit":3}`), argsDone(whole), itemDone(utils.Ptr(whole))},
		"whole on item_added, then streamed (Gemini)": {added(utils.Ptr(whole)), delta(whole), argsDone(whole), itemDone(utils.Ptr(whole))},
		"whole on item_added only (cached)":           {added(utils.Ptr(whole)), itemDone(nil)},
		"stated only at the end":                      {added(nil), argsDone(whole), itemDone(nil)},
		"streamed short of the end":                   {added(utils.Ptr("")), delta(`{"query":"quarterly revenue",`), itemDone(utils.Ptr(whole))},
	} {
		t.Run(name, func(t *testing.T) {
			translator := NewTranslator("thread", "run")
			var args string
			var ended bool
			for _, chunk := range chunks {
				for _, event := range translator.Translate(chunk) {
					switch e := event.(type) {
					case *ToolCallArgsEvent:
						require.False(t, ended, "arguments arrive before the call ends")
						assert.Equal(t, "call-1", e.ToolCallID)
						args += e.Delta
					case *ToolCallEndEvent:
						ended = true
					}
				}
			}
			require.True(t, ended)
			assert.JSONEq(t, whole, args)
		})
	}
}
