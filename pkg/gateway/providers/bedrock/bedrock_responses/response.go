package bedrock_responses

import (
	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// ConverseResponse is the response from the Bedrock Converse API.
type ConverseResponse struct {
	Output     ConverseOutput   `json:"output"`
	StopReason string           `json:"stopReason"` // "end_turn", "tool_use", "max_tokens", "stop_sequence", "content_filtered"
	Usage      ConverseUsage    `json:"usage"`
	Metrics    *ConverseMetrics `json:"metrics,omitempty"`
}

type ConverseOutput struct {
	Message *ConverseMessage `json:"message,omitempty"`
}

type ConverseUsage struct {
	InputTokens           int `json:"inputTokens"`
	OutputTokens          int `json:"outputTokens"`
	TotalTokens           int `json:"totalTokens"`
	CacheReadInputTokens  int `json:"cacheReadInputTokens,omitempty"`
	CacheWriteInputTokens int `json:"cacheWriteInputTokens,omitempty"`
}

// nativeUsage normalizes Converse token accounting onto the native Usage
// contract, where InputTokens is the whole prompt and CachedTokens a subset of
// it.
//
// Converse uses split cache accounting for every model family: inputTokens
// counts only the tokens that were neither read from nor written to the cache,
// and AWS documents the real prompt as
//
//	inputTokens + cacheReadInputTokens + cacheWriteInputTokens
//
// The reported totalTokens is no help here — it is inputTokens+outputTokens and
// excludes the cache counts just as inputTokens does, so it cannot be used to
// detect whether they are already folded in. Add them unconditionally; they are
// zero when caching is off, which makes this a no-op for uncached requests.
//
// https://docs.aws.amazon.com/bedrock/latest/userguide/prompt-caching.html
func nativeUsage(u ConverseUsage) *responses.Usage {
	input := u.InputTokens + u.CacheReadInputTokens + u.CacheWriteInputTokens

	out := &responses.Usage{
		InputTokens:  input,
		OutputTokens: u.OutputTokens,
		TotalTokens:  input + u.OutputTokens,
	}
	out.InputTokensDetails.CachedTokens = u.CacheReadInputTokens

	return out
}

type ConverseMetrics struct {
	LatencyMs int `json:"latencyMs"`
}

// ToNativeResponse converts a Bedrock Converse response to the native SDK response format.
func (r *ConverseResponse) ToNativeResponse(model string) *responses.Response {
	var output []responses.OutputMessageUnion

	if r.Output.Message != nil {
		for _, block := range r.Output.Message.Content {
			if block.Text != nil {
				output = append(output, responses.OutputMessageUnion{
					OfOutputMessage: &responses.OutputMessage{
						ID:   responses.NewOutputItemMessageID(),
						Role: constants.RoleAssistant,
						Content: &responses.OutputContent{
							{
								OfOutputText: &responses.OutputTextContent{
									Text:        *block.Text,
									Annotations: []responses.Annotation{},
								},
							},
						},
					},
				})
			}

			if block.ToolUse != nil {
				args, err := sonic.Marshal(block.ToolUse.Input)
				if err != nil {
					args = []byte("{}")
				}

				output = append(output, responses.OutputMessageUnion{
					OfFunctionCall: &responses.FunctionCallMessage{
						ID:        responses.NewOutputItemFunctionCallID(),
						CallID:    block.ToolUse.ToolUseId,
						Name:      block.ToolUse.Name,
						Arguments: string(args),
					},
				})
			}
			if block.ReasoningContent != nil {
				rm := &responses.ReasoningMessage{
					ID: responses.NewOutputItemReasoningID(),
				}
				if block.ReasoningContent.ReasoningText != nil {
					rm.Summary = []responses.SummaryTextContent{
						{Text: block.ReasoningContent.ReasoningText.Text},
					}
					if block.ReasoningContent.ReasoningText.Signature != "" {
						sig := block.ReasoningContent.ReasoningText.Signature
						rm.EncryptedContent = &sig
					}
				}
				if block.ReasoningContent.RedactedContent != nil {
					rm.EncryptedContent = block.ReasoningContent.RedactedContent
				}
				output = append(output, responses.OutputMessageUnion{
					OfReasoning: rm,
				})
			}
		}
	}

	usage := nativeUsage(r.Usage)

	return &responses.Response{
		ID:     responses.NewOutputItemMessageID(),
		Model:  model,
		Output: output,
		Usage:  usage,
		Metadata: map[string]any{
			"stop_reason": r.StopReason,
		},
	}
}
