package openaicompat

import (
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// ToNativeResponse translates a chat completion into the native Responses
// shape. The first choice is used; n > 1 is not expressible natively.
func (in *ChatResponse) ToNativeResponse() *responses.Response {
	out := &responses.Response{
		ID:     in.ID,
		Model:  in.Model,
		Output: []responses.OutputMessageUnion{},
		Usage:  in.Usage.ToNativeUsage(),
	}

	if len(in.Choices) == 0 {
		return out
	}

	choice := in.Choices[0]
	out.Metadata = map[string]any{"finish_reason": choice.FinishReason}
	out.Output = messageToNativeOutput(choice.Message)

	return out
}

func messageToNativeOutput(msg Message) []responses.OutputMessageUnion {
	out := []responses.OutputMessageUnion{}

	// Reasoning first: it is what the model produced first, and the agent
	// loop replays output order as history order.
	if msg.ReasoningContent != "" {
		out = append(out, responses.OutputMessageUnion{
			OfReasoning: &responses.ReasoningMessage{
				ID:      responses.NewOutputItemReasoningID(),
				Summary: []responses.SummaryTextContent{{Text: msg.ReasoningContent}},
			},
		})
	}

	if text := msg.Content.Text(); text != "" {
		out = append(out, responses.OutputMessageUnion{
			OfOutputMessage: &responses.OutputMessage{
				ID:   responses.NewOutputItemMessageID(),
				Role: constants.RoleAssistant,
				Content: &responses.OutputContent{
					{OfOutputText: &responses.OutputTextContent{
						Text:        text,
						Annotations: []responses.Annotation{},
					}},
				},
			},
		})
	}

	for _, call := range msg.ToolCalls {
		args := call.Function.Arguments
		if args == "" {
			args = "{}"
		}

		out = append(out, responses.OutputMessageUnion{
			OfFunctionCall: &responses.FunctionCallMessage{
				ID:        responses.NewOutputItemFunctionCallID(),
				CallID:    call.ID,
				Name:      call.Function.Name,
				Arguments: args,
			},
		})
	}

	return out
}

// ToNativeUsage normalizes chat completion token accounting onto the native
// contract: InputTokens is the whole prompt and CachedTokens is the subset of
// it served from cache (see responses.Usage).
func (u *Usage) ToNativeUsage() *responses.Usage {
	if u == nil {
		return nil
	}

	out := &responses.Usage{
		InputTokens:  u.PromptTokens,
		OutputTokens: u.CompletionTokens,
		TotalTokens:  u.TotalTokens,
	}

	if out.TotalTokens == 0 {
		out.TotalTokens = out.InputTokens + out.OutputTokens
	}

	switch {
	case u.PromptTokensDetails != nil:
		out.InputTokensDetails.CachedTokens = u.PromptTokensDetails.CachedTokens
	case u.PromptCacheHitTokens > 0:
		out.InputTokensDetails.CachedTokens = u.PromptCacheHitTokens
	}

	if u.CompletionTokensDetails != nil {
		out.OutputTokensDetails.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
	}

	return out
}

func (e *ChatError) ToNativeError() *responses.Error {
	if e == nil {
		return nil
	}

	return &responses.Error{
		Type:    e.Type,
		Message: e.Message,
		Param:   e.Param,
		Code:    e.Code,
	}
}

func nativeUsageOrEmpty(u *Usage) responses.Usage {
	if native := u.ToNativeUsage(); native != nil {
		return *native
	}

	return responses.Usage{}
}
