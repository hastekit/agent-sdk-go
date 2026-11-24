package testutil

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// Step is one scripted model turn: a call to Tool with Arguments, or, when
// Tool is empty, a final answer of Text.
type Step struct {
	Tool      string
	Arguments string
	Text      string
}

// SeenRequest is what one model call was offered.
type SeenRequest struct {
	Tools        []string
	Instructions string
	// Texts are the input's text parts and tool outputs, in order.
	Texts []string
}

// ScriptedProvider is an llm.Provider that streams its steps in order, one per
// call, and records what each call was offered. Calls past the script answer
// "done". The rest of llm.Provider is the nil embedded interface.
type ScriptedProvider struct {
	llm.Provider

	mu    sync.Mutex
	steps []Step
	seen  []SeenRequest
}

func NewScriptedProvider(steps ...Step) *ScriptedProvider {
	return &ScriptedProvider{steps: steps}
}

// Seen returns the requests made so far.
func (p *ScriptedProvider) Seen() []SeenRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.seen)
}

func (p *ScriptedProvider) NewStreamingResponses(_ context.Context, in *responses.Request) (chan *responses.ResponseChunk, error) {
	p.mu.Lock()
	seen := SeenRequest{}
	for _, tool := range in.Tools {
		if tool.OfFunction != nil {
			seen.Tools = append(seen.Tools, tool.OfFunction.Name)
		}
	}
	slices.Sort(seen.Tools)
	if in.Instructions != nil {
		seen.Instructions = *in.Instructions
	}
	addTexts := func(content responses.InputContent) {
		for _, part := range content {
			if part.OfInputText != nil {
				seen.Texts = append(seen.Texts, part.OfInputText.Text)
			}
		}
	}
	for _, item := range in.Input.OfInputMessageList {
		// A message that crossed a JSON boundary, as durable runtimes' do,
		// comes back as the union's easy form.
		if item.OfInputMessage != nil {
			addTexts(item.OfInputMessage.Content)
		}
		if item.OfEasyInput != nil {
			if item.OfEasyInput.Content.OfString != nil {
				seen.Texts = append(seen.Texts, *item.OfEasyInput.Content.OfString)
			}
			addTexts(item.OfEasyInput.Content.OfInputMessageList)
		}
		if item.OfFunctionCallOutput != nil && item.OfFunctionCallOutput.Output.OfString != nil {
			seen.Texts = append(seen.Texts, *item.OfFunctionCallOutput.Output.OfString)
		}
	}
	turn := len(p.seen)
	p.seen = append(p.seen, seen)
	step := Step{Text: "done"}
	if turn < len(p.steps) {
		step = p.steps[turn]
	}
	p.mu.Unlock()

	id := fmt.Sprintf("item_%d", turn)
	var item responses.ChunkOutputItemData
	if step.Tool != "" {
		callID := fmt.Sprintf("call_%d", turn)
		arguments := step.Arguments
		if arguments == "" {
			arguments = "{}"
		}
		item = responses.ChunkOutputItemData{Type: "function_call", Id: id, Status: "completed", CallID: &callID, Name: &step.Tool, Arguments: &arguments}
	} else {
		content := responses.ChunkOutputItemContent{{OfOutputText: &responses.OutputTextContent{Type: constants.ContentTypeOutputText(""), Text: step.Text}}}
		item = responses.ChunkOutputItemData{Type: "message", Id: id, Status: "completed", Role: constants.RoleAssistant, Content: &content}
	}
	stream := make(chan *responses.ResponseChunk, 2)
	stream <- &responses.ResponseChunk{OfOutputItemDone: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{Item: item}}
	stream <- &responses.ResponseChunk{OfResponseCompleted: &responses.ChunkResponse[constants.ChunkTypeResponseCompleted]{}}
	close(stream)
	return stream, nil
}
