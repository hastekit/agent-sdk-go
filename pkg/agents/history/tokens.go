package history

import (
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// Token estimation constants. These produce a rough figure, deliberately: the
// estimate exists to cover the messages appended since the last provider usage
// report, and it is replaced by that exact number as soon as the next report
// arrives. It never has to be right for long.
const (
	// charsPerToken is the usual rule of thumb for English prose. It runs low
	// on code and structured text, which errs toward summarizing sooner —
	// the safe direction, since the cost of a needless summary is one call and
	// the cost of a missed one is a rejected request.
	charsPerToken = 4

	// perMessageOverhead covers the role marker and delimiters a provider wraps
	// around each item.
	perMessageOverhead = 4

	// imageTokens is a flat nominal cost per image. Images are billed by
	// dimension, not by payload size, so measuring the base64 data URL — often
	// hundreds of thousands of characters — would swamp the whole estimate.
	// A flat figure is wrong by a bounded amount; the alternative is wrong by
	// orders of magnitude.
	imageTokens = 1500

	// fileTokens is the same idea for an attached file referenced by id or URL,
	// whose content the provider expands on its side and we cannot see.
	fileTokens = 1000
)

// estimateBundleTokens approximates the context a bundle will occupy once it is
// sent. See EstimateTokens for why an approximation is enough.
func estimateBundleTokens(m Message) int {
	total := 0
	for _, msg := range m.Messages {
		total += estimateMessageTokens(msg)
	}
	return total
}

// estimateMessageTokens approximates one provider input item.
func estimateMessageTokens(msg responses.InputMessageUnion) int {
	total := perMessageOverhead

	switch {
	case msg.OfEasyInput != nil:
		if s := msg.OfEasyInput.Content.OfString; s != nil {
			total += textTokens(*s)
		}
		for _, c := range msg.OfEasyInput.Content.OfInputMessageList {
			total += contentTokens(c)
		}

	case msg.OfInputMessage != nil:
		for _, c := range msg.OfInputMessage.Content {
			total += contentTokens(c)
		}

	case msg.OfOutputMessage != nil:
		if msg.OfOutputMessage.Content != nil {
			for _, c := range *msg.OfOutputMessage.Content {
				if c.OfOutputText != nil {
					total += textTokens(c.OfOutputText.Text)
				}
			}
		}

	case msg.OfFunctionCall != nil:
		total += textTokens(msg.OfFunctionCall.Name) + textTokens(msg.OfFunctionCall.Arguments)

	case msg.OfFunctionCallOutput != nil:
		if s := msg.OfFunctionCallOutput.Output.OfString; s != nil {
			total += textTokens(*s)
		}
		for _, c := range msg.OfFunctionCallOutput.Output.OfList {
			total += contentTokens(c)
		}

	case msg.OfReasoning != nil:
		for _, s := range msg.OfReasoning.Summary {
			total += textTokens(s.Text)
		}
		// EncryptedContent is an opaque blob the provider round-trips; it is
		// not re-tokenized as text, so its length is not counted here.

	case msg.OfCodeInterpreterCall != nil:
		total += textTokens(msg.OfCodeInterpreterCall.Code)
		for _, o := range msg.OfCodeInterpreterCall.Outputs {
			total += textTokens(o.Logs)
		}
	}

	return total
}

func contentTokens(c responses.InputContentUnion) int {
	switch {
	case c.OfInputText != nil:
		return textTokens(c.OfInputText.Text)
	case c.OfOutputText != nil:
		return textTokens(c.OfOutputText.Text)
	case c.OfInputImage != nil:
		return imageTokens
	case c.OfInputFile != nil:
		return fileTokens
	}
	return 0
}

func textTokens(s string) int {
	if s == "" {
		return 0
	}
	// Round up so short strings never estimate to zero.
	return (len(s) + charsPerToken - 1) / charsPerToken
}
