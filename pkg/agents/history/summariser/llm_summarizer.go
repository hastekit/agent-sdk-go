package summariser

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/history"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/messages"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/hastekit-sdk-go/pkg/utils"
)

type LLMHistorySummarizer struct {
	llm             llm.Provider
	instruction     agents.SystemPromptProvider
	tokenThreshold  int
	keepRecentCount int // Number of recent messages to keep unsummarized
	parameters      responses.Parameters
}

type LLMHistorySummarizerOptions struct {
	LLM             llm.Provider
	Instruction     agents.SystemPromptProvider
	TokenThreshold  int
	KeepRecentCount int // Optional: defaults to 5
	Parameters      responses.Parameters
}

func NewLLMHistorySummarizer(opts *LLMHistorySummarizerOptions) *LLMHistorySummarizer {
	keepRecentCount := 5
	if opts.KeepRecentCount > 0 {
		keepRecentCount = opts.KeepRecentCount
	}

	return &LLMHistorySummarizer{
		llm:             opts.LLM,
		instruction:     opts.Instruction,
		tokenThreshold:  opts.TokenThreshold,
		keepRecentCount: keepRecentCount,
		parameters:      opts.Parameters,
	}
}

// shouldSummarize determines if summarization is needed and returns the index from which to keep messages.
// Returns (shouldSummarize, keepFromIndex)
// If shouldSummarize is false, keepFromIndex is -1
// If shouldSummarize is true, keepFromIndex is the index from which to keep messages (messages[keepFromIndex:] are kept)
func (s *LLMHistorySummarizer) shouldSummarize(ctx context.Context, runs []Run, contextTokens int) (bool, int) {
	if contextTokens < s.tokenThreshold {
		return false, -1
	}

	// Need at least keepRecentCount + 1 messages to summarize (keep some, summarize the rest)
	if len(runs) <= s.keepRecentCount {
		return false, -1
	}

	// Strategy: Keep the most recent messages and summarize everything before them
	// We'll keep the last keepRecentCount messages (or fewer if that's all we have)
	keepFromIndex := len(runs) - s.keepRecentCount
	if keepFromIndex < 0 {
		keepFromIndex = 0
	}

	// Only summarize if we have messages before keepFromIndex to summarize
	if keepFromIndex > 0 {
		return true, keepFromIndex
	}

	return false, -1
}

type Run struct {
	RunID    string
	Messages []messages.Message
}

func (s *LLMHistorySummarizer) Summarize(ctx context.Context, msgIdToRunId map[string]string, msgs []messages.Message, contextTokens int) (*history.SummaryResult, error) {
	// Group messages using their run id
	runs := []Run{}
	runIdsSeen := []string{}
	for _, msg := range msgs {
		runId := msgIdToRunId[msg.ID]

		if !slices.Contains(runIdsSeen, runId) {
			runs = append(runs, Run{
				RunID:    runId,
				Messages: []messages.Message{msg},
			})
			runIdsSeen = append(runIdsSeen, runId)
			continue
		}

		run := &runs[len(runs)-1]
		run.Messages = append(run.Messages, msg)
	}

	shouldSummarize, keepFromIndex := s.shouldSummarize(ctx, runs, contextTokens)
	if !shouldSummarize {
		return nil, nil
	}

	// Messages to summarize are from the beginning up to (but not including) keepFromIndex
	runsToSummarize := runs[:keepFromIndex]
	runsToKeep := runs[keepFromIndex:]

	// Skip if no messages to summarize
	if len(runsToSummarize) == 0 {
		return nil, nil
	}

	// Find the last message ID that was summarized
	// This is the last message in msgsToSummarize that has an ID
	var lastSummarizedMessageID string
	for i := len(runsToSummarize) - 1; i >= 0; i-- {
		if runsToSummarize[i].RunID != "" {
			lastSummarizedMessageID = runsToSummarize[i].RunID
			break
		}
	}

	messagesToSummarize := []responses.InputMessageUnion{}
	for _, run := range runsToSummarize {
		for _, msg := range run.Messages {
			messagesToSummarize = append(messagesToSummarize, msg.Messages...)
		}
	}

	messagesToKeep := []messages.Message{}
	for _, run := range runsToKeep {
		messagesToKeep = append(messagesToKeep, run.Messages...)
	}

	// Get instruction for summarization
	var instruction string
	var err error
	if s.instruction != nil {
		instruction, err = s.instruction.GetPrompt(ctx, nil)
		if err != nil {
			return nil, err
		}
	} else {
		slog.WarnContext(ctx, "summarizer is missing system instructions, skipping summarization")
		return nil, nil
	}

	// Format history for summarization
	var historyBuilder strings.Builder
	for _, msg := range messagesToSummarize {
		switch {
		case msg.OfEasyInput != nil:
			if msg.OfEasyInput.Content.OfString != nil {
				historyBuilder.WriteString(fmt.Sprintf("%s: %s\n", msg.OfEasyInput.Role, *msg.OfEasyInput.Content.OfString))
			} else if msg.OfEasyInput.Content.OfInputMessageList != nil {
				for _, subMsg := range msg.OfEasyInput.Content.OfInputMessageList {
					if subMsg.OfInputText != nil {
						historyBuilder.WriteString(fmt.Sprintf("%s: %s\n", msg.OfEasyInput.Role, subMsg.OfInputText.Text))
					} else if subMsg.OfOutputText != nil {
						historyBuilder.WriteString(fmt.Sprintf("%s: [Tool Call: %s]\n", msg.OfEasyInput.Role, subMsg.OfOutputText.Text))
					}
				}
			}

		case msg.OfInputMessage != nil:
			for _, content := range msg.OfInputMessage.Content {
				if content.OfInputText != nil {
					historyBuilder.WriteString(fmt.Sprintf("%s: %s\n", msg.OfInputMessage.Role, content.OfInputText.Text))
				}

				if content.OfOutputText != nil {
					historyBuilder.WriteString(fmt.Sprintf("%s: %s\n", msg.OfInputMessage.Role, content.OfOutputText.Text))
				}
			}

		case msg.OfOutputMessage != nil:
			for _, content := range *msg.OfOutputMessage.Content {
				if content.OfOutputText != nil {
					historyBuilder.WriteString(fmt.Sprintf("%s: %s\n", msg.OfOutputMessage.Role, content.OfOutputText.Text))
				}
			}

		case msg.OfFunctionCall != nil:
			historyBuilder.WriteString(fmt.Sprintf("[Tool Call: %s]\n", msg.OfFunctionCall.Name))

		case msg.OfFunctionCallOutput != nil:
			if msg.OfFunctionCallOutput.Output.OfString != nil {
				historyBuilder.WriteString(fmt.Sprintf("[Tool Result: %s]\n", *msg.OfFunctionCallOutput.Output.OfString))
			}
		}

	}

	userMsg := responses.InputMessageUnion{OfInputMessage: &responses.InputMessage{
		Role: constants.RoleUser,
		Content: responses.InputContent{
			{
				OfInputText: &responses.InputTextContent{Text: fmt.Sprintf("Please summarize the following conversation history, preserving important context, decisions, and information that would be needed for future interactions:\n\n%s", historyBuilder.String())},
			},
		},
	}}

	// Invoke LLM for summarization
	resp, err := s.llm.NewResponses(ctx, &responses.Request{
		Instructions: utils.Ptr(instruction),
		Input: responses.InputUnion{
			OfInputMessageList: responses.InputMessageList{userMsg},
		},
		Parameters: s.parameters,
	})
	if err != nil {
		return nil, err
	}

	summaryMsg := resp.Output

	// Extract text from summary
	var summaryText string
	for _, msg := range summaryMsg {
		for _, content := range *msg.OfOutputMessage.Content {
			summaryText += content.OfOutputText.Text
		}
	}

	var summaryId string
	if len(summaryMsg) > 0 {
		if summaryMsg[0].OfOutputMessage != nil {
			summaryId = summaryMsg[0].OfOutputMessage.ID
		}
	}

	if summaryText == "" {
		return nil, fmt.Errorf("empty summary generated")
	}

	// Return summary as SystemMessage
	summaryMessage := responses.InputMessageUnion{
		OfInputMessage: &responses.InputMessage{
			ID:      summaryId,
			Role:    constants.RoleSystem,
			Content: responses.InputContent{{OfInputText: &responses.InputTextContent{Text: fmt.Sprintf("Previous conversation summary: %s", summaryText)}}},
		},
	}

	// Generate summary ID
	summaryID := uuid.NewString()

	return &history.SummaryResult{
		Summary:                 &messages.Message{Messages: []responses.InputMessageUnion{summaryMessage}},
		LastSummarizedMessageID: lastSummarizedMessageID,
		SummaryID:               summaryID,
		MessagesToKeep:          messagesToKeep,
		// Summarizing costs real tokens. Reporting them here is what keeps the
		// run's usage total from silently excluding the work done to shrink it.
		Usage: resp.Usage,
	}, nil
}
