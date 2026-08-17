package anthropic_responses

import (
	"fmt"
	"time"

	"github.com/bytedance/sonic"
	"github.com/google/uuid"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	responses2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

func (in *Request) ToNativeRequest() *responses2.Request {
	instructions := ""
	for _, sys := range in.System {
		instructions += sys.Text
	}

	out := &responses2.Request{
		Model:        in.Model,
		Input:        MessagesToNativeMessages(in.Messages),
		Tools:        ToolsToNativeTools(in.Tools),
		Instructions: utils.Ptr(instructions),
		Parameters: responses2.Parameters{
			Background:      utils.Ptr(false),
			MaxOutputTokens: &in.MaxTokens,
			Temperature:     in.Temperature,
			TopP:            in.TopP,
			TopLogprobs:     in.TopK,
			Metadata:        in.Metadata,
			Stream:          in.Stream,
			Include:         []responses2.Includable{},
			ExtraFields:     in.ExtraFields.AsMap(),
		},
	}

	if in.Thinking != nil {
		if in.Thinking.Type != nil && *in.Thinking.Type == "enabled" {
			out.Reasoning = &responses2.ReasoningParam{
				Summary: utils.Ptr("auto"),
			}

			if in.Thinking.BudgetTokens != nil {
				switch *in.Thinking.BudgetTokens {
				case 10000:
					out.Reasoning.Effort = utils.Ptr("xhigh")
				case 6000:
					out.Reasoning.Effort = utils.Ptr("high")
				case 3000:
					out.Reasoning.Effort = utils.Ptr("medium")
				case 1024:
					out.Reasoning.Effort = utils.Ptr("low")
				}
			}

			out.Include = append(out.Include, responses2.IncludableReasoningEncryptedContent)
		}

		if in.Thinking.Type != nil && *in.Thinking.Type == "adaptive" {
			out.Reasoning = &responses2.ReasoningParam{
				Summary: utils.Ptr("auto"),
			}

			if in.OutputConfig != nil && in.OutputConfig.Effort != nil {
				switch *in.OutputConfig.Effort {
				case "low":
					out.Reasoning.Effort = utils.Ptr("low")
				case "medium":
					out.Reasoning.Effort = utils.Ptr("medium")
				case "high":
					out.Reasoning.Effort = utils.Ptr("high")
				case "xhigh":
					out.Reasoning.Effort = utils.Ptr("xhigh")
				}
			}
		}
	}

	if in.OutputFormat != nil {
		out.Text = &responses2.TextFormat{
			Format: in.OutputFormat,
		}
		out.Text.Format["name"] = "structured_output"
	}

	return out
}

func (in Role) ToNativeRole() constants.Role {
	switch in {
	case RoleUser:
		return constants.RoleUser
	case RoleAssistant:
		return constants.RoleAssistant
	}

	return constants.RoleAssistant
}

func ToolsToNativeTools(in []ToolUnion) []responses2.ToolUnion {
	out := make([]responses2.ToolUnion, len(in))
	for idx, tool := range in {
		out[idx] = tool.ToNative()
	}

	return out
}

func (in *ToolUnion) ToNative() responses2.ToolUnion {
	out := responses2.ToolUnion{}

	if in.OfCustomTool != nil {
		out.OfFunction = &responses2.FunctionTool{
			Type:        "function",
			Name:        in.OfCustomTool.Name,
			Description: in.OfCustomTool.Description,
			Parameters:  in.OfCustomTool.InputSchema,
		}
	}

	if in.OfWebSearchTool != nil {
		out.OfWebSearch = &responses2.WebSearchTool{
			Type: "web_search",
			Filters: &responses2.WebSearchToolFilters{
				AllowedDomains: in.OfWebSearchTool.AllowedDomains,
			},
		}

		if in.OfWebSearchTool.UserLocation != nil {
			out.OfWebSearch.UserLocation = &responses2.WebSearchToolUserLocation{
				Type:     in.OfWebSearchTool.UserLocation.Type,
				Country:  in.OfWebSearchTool.UserLocation.Country,
				City:     in.OfWebSearchTool.UserLocation.City,
				Region:   in.OfWebSearchTool.UserLocation.Region,
				Timezone: in.OfWebSearchTool.UserLocation.Timezone,
			}
		}
	}

	if in.OfCodeExecutionTool != nil {
		out.OfCodeExecution = &responses2.CodeExecutionTool{}
	}

	return out
}

func CitationsToNativeAnnotations(citations []Citation) []responses2.Annotation {
	var annotations []responses2.Annotation

	for _, citation := range citations {
		annotations = append(annotations, responses2.Annotation{
			Type:       "url_citation",
			Title:      citation.Title,
			URL:        citation.Url,
			StartIndex: 0,
			EndIndex:   0,
			ExtraParams: map[string]any{
				"Anthropic": citation,
			},
		})
	}

	return annotations
}

func ThinkingEffortToNativeReasoningEffort(effort *string) *string {
	if effort == nil {
		return nil
	}

	switch *effort {
	case "low":
		return utils.Ptr("low")
	case "medium":
		return utils.Ptr("medium")
	case "high":
		return utils.Ptr("high")
	case "max":
		return utils.Ptr("xhigh")
	default:
		return utils.Ptr("low")
	}
}

func MessagesToNativeMessages(msgs []MessageUnion) responses2.InputUnion {
	out := responses2.InputUnion{
		OfString:           nil,
		OfInputMessageList: responses2.InputMessageList{},
	}

	for _, msg := range msgs {
		out.OfInputMessageList = append(out.OfInputMessageList, msg.ToNativeMessage())
	}

	return out
}

func (msg *MessageUnion) ToNativeMessage() responses2.InputMessageUnion {
	var previousServerToolUse *ServerToolUseContent

	if msg.Content.OfString != nil {
		return responses2.InputMessageUnion{
			OfInputMessage: &responses2.InputMessage{
				Role: msg.Role.ToNativeRole(),
				Content: responses2.InputContent{
					{
						OfInputText: &responses2.InputTextContent{
							Text: *msg.Content.OfString,
						},
					},
				},
			},
		}
	}

	contents := responses2.InputContent{}
	for _, content := range msg.Content.OfList {
		if content.OfToolUse != nil {
			argsBuf, err := sonic.Marshal(content.OfToolUse.Input)
			if err != nil {
				argsBuf = []byte("{}")
			}

			return responses2.InputMessageUnion{
				OfFunctionCall: &responses2.FunctionCallMessage{
					ID:        content.OfToolUse.ID,
					CallID:    content.OfToolUse.ID,
					Name:      content.OfToolUse.Name,
					Arguments: string(argsBuf),
				},
			}
		}

		if content.OfToolResult != nil {
			outputs := responses2.FunctionCallOutputContentUnion{
				OfString: nil,
				OfList:   responses2.InputContent{},
			}

			if content.OfToolResult.Content.OfString != nil {
				outputs.OfString = content.OfToolResult.Content.OfString
			}

			// TODO: outputContent can be text, image, search result or document
			for _, outputContent := range content.OfToolResult.Content.OfList {
				if outputContent.OfText != nil {
					outputs.OfList = append(outputs.OfList, responses2.InputContentUnion{
						OfInputText: &responses2.InputTextContent{
							Text: outputContent.OfText.Text,
						},
					})
				}
			}

			return responses2.InputMessageUnion{
				OfFunctionCallOutput: &responses2.FunctionCallOutputMessage{
					ID:     content.OfToolResult.ToolUseID,
					CallID: content.OfToolResult.ToolUseID,
					Output: outputs,
				},
			}
		}

		if content.OfThinking != nil {
			return responses2.InputMessageUnion{
				OfReasoning: &responses2.ReasoningMessage{
					ID: uuid.NewString(),
					Summary: []responses2.SummaryTextContent{{
						Text: content.OfThinking.Thinking,
					}},
					EncryptedContent: utils.Ptr(content.OfThinking.Signature),
				},
			}
		}

		if content.OfRedactedThinking != nil {
			return responses2.InputMessageUnion{
				OfReasoning: &responses2.ReasoningMessage{
					ID:               uuid.NewString(),
					Summary:          nil,
					EncryptedContent: utils.Ptr(content.OfRedactedThinking.Data),
				},
			}
		}

		if content.OfServerToolUse != nil {
			previousServerToolUse = content.OfServerToolUse
		}

		if content.OfWebSearchResult != nil {
			if previousServerToolUse != nil && previousServerToolUse.Name == "web_search" {
				id := previousServerToolUse.Id
				query := previousServerToolUse.Input.Query
				var sources []responses2.WebSearchCallActionOfSearchSource
				for _, searchResultContent := range content.OfWebSearchResult.Content {
					sources = append(sources, responses2.WebSearchCallActionOfSearchSource{
						Type: "url",
						URL:  searchResultContent.Url,
						ExtraParams: map[string]any{
							"Anthropic": searchResultContent,
						},
					})
				}

				return responses2.InputMessageUnion{
					OfWebSearchCall: &responses2.WebSearchCallMessage{
						ID: id,
						Action: responses2.WebSearchCallActionUnion{
							OfSearch: &responses2.WebSearchCallActionOfSearch{
								Queries: []string{
									query,
								},
								Query:   query,
								Sources: sources,
							},
						},
						Status: "completed",
					},
				}
			}
		}

		if content.OfBashCodeExecutionToolResult != nil {
			if previousServerToolUse != nil && previousServerToolUse.Name == "bash_code_execution" {
				id := previousServerToolUse.Id
				cmd := previousServerToolUse.Input.Command
				cmdOut := content.OfBashCodeExecutionToolResult.Content.Stdout
				if content.OfBashCodeExecutionToolResult.Content.ReturnCode != 0 {
					cmd = content.OfBashCodeExecutionToolResult.Content.Stderr
				}

				return responses2.InputMessageUnion{
					OfCodeInterpreterCall: &responses2.CodeInterpreterCallMessage{
						ID:          id,
						Status:      "completed",
						Code:        cmd,
						ContainerID: "",
						Outputs: []responses2.CodeInterpreterCallOutputParam{
							{
								Type: "logs",
								Logs: cmdOut,
							},
						},
					},
				}
			}
		}

		if content.OfText != nil {
			if content.OfText.Citations != nil {
				contents = append(contents, responses2.InputContentUnion{
					OfOutputText: &responses2.OutputTextContent{
						Text:        content.OfText.Text,
						Annotations: CitationsToNativeAnnotations(content.OfText.Citations), // Convert citation to annotation
					},
				})
			} else {
				contents = append(contents, responses2.InputContentUnion{
					OfInputText: &responses2.InputTextContent{
						Text: content.OfText.Text,
					},
				})
			}
		}

		if content.OfImage != nil {
			switch content.OfImage.Source.Type {
			case "base64":
				contents = append(contents, responses2.InputContentUnion{
					OfInputImage: &responses2.InputImageContent{
						ImageURL: utils.Ptr(fmt.Sprintf("data:%s;base64,%s", *content.OfImage.Source.MediaType, *content.OfImage.Source.Data)),
					},
				})
			}
		}
	}

	return responses2.InputMessageUnion{
		OfInputMessage: &responses2.InputMessage{
			Role:    msg.Role.ToNativeRole(),
			Content: contents,
		},
	}
}

func (in *Response) ToNativeResponse() *responses2.Response {
	output := []responses2.OutputMessageUnion{}

	var previousWebSearchCall *ServerToolUseContent

	for _, content := range in.Content {
		if content.OfText != nil {
			output = append(output, responses2.OutputMessageUnion{
				OfOutputMessage: &responses2.OutputMessage{
					Role: constants.RoleAssistant,
					Content: &responses2.OutputContent{
						{
							OfOutputText: &responses2.OutputTextContent{
								Text:        content.OfText.Text,
								Annotations: CitationsToNativeAnnotations(content.OfText.Citations),
							},
						},
					},
				},
			})
		}

		if content.OfToolUse != nil {
			args, err := sonic.Marshal(content.OfToolUse.Input)
			if err != nil {
				args = []byte("{}")
			}

			output = append(output, responses2.OutputMessageUnion{
				OfFunctionCall: &responses2.FunctionCallMessage{
					ID:        content.OfToolUse.ID,
					CallID:    content.OfToolUse.ID,
					Name:      content.OfToolUse.Name,
					Arguments: string(args),
				},
			})
		}

		if content.OfThinking != nil {
			output = append(output, responses2.OutputMessageUnion{
				OfReasoning: &responses2.ReasoningMessage{
					Summary: []responses2.SummaryTextContent{
						{Text: content.OfThinking.Thinking},
					},
					EncryptedContent: utils.Ptr(content.OfThinking.Signature),
				},
			})
		}

		// Redacted thinking is also converted into reasoning message only.
		// Just that it won't have a summary.
		if content.OfRedactedThinking != nil {
			output = append(output, responses2.OutputMessageUnion{
				OfReasoning: &responses2.ReasoningMessage{
					EncryptedContent: utils.Ptr(content.OfRedactedThinking.Data),
				},
			})
		}

		// We simply store the service_tool_use message for later reference
		if content.OfServerToolUse != nil {
			previousWebSearchCall = content.OfServerToolUse
		}

		if content.OfWebSearchResult != nil && previousWebSearchCall != nil && previousWebSearchCall.Name == "web_search" {
			sources := []responses2.WebSearchCallActionOfSearchSource{}
			for _, searchResultContent := range content.OfWebSearchResult.Content {
				sources = append(sources, responses2.WebSearchCallActionOfSearchSource{
					Type: "url",
					URL:  searchResultContent.Url,
					ExtraParams: map[string]any{
						"Anthropic": searchResultContent,
					},
				})
			}

			output = append(output, responses2.OutputMessageUnion{
				OfWebSearchCall: &responses2.WebSearchCallMessage{
					ID: previousWebSearchCall.Id,
					Action: responses2.WebSearchCallActionUnion{
						OfSearch: &responses2.WebSearchCallActionOfSearch{
							Queries: []string{
								previousWebSearchCall.Input.Query,
							},
							Query:   previousWebSearchCall.Input.Query,
							Sources: sources,
						},
					},
					Status: "completed",
				},
			})

			previousWebSearchCall = nil
		}

		if content.OfBashCodeExecutionToolResult != nil {
			if previousWebSearchCall != nil && previousWebSearchCall.Name == "bash_code_execution" {
				id := previousWebSearchCall.Id
				cmd := previousWebSearchCall.Input.Command
				cmdOut := content.OfBashCodeExecutionToolResult.Content.Stdout
				if content.OfBashCodeExecutionToolResult.Content.ReturnCode != 0 {
					cmd = content.OfBashCodeExecutionToolResult.Content.Stderr
				}

				output = append(output, responses2.OutputMessageUnion{
					OfCodeInterpreterCall: &responses2.CodeInterpreterCallMessage{
						ID:          id,
						Status:      "completed",
						Code:        cmd,
						ContainerID: "",
						Outputs: []responses2.CodeInterpreterCallOutputParam{
							{
								Type: "logs",
								Logs: cmdOut,
							},
						},
					},
				})

				previousWebSearchCall = nil
			}
		}
	}

	return &responses2.Response{
		ID:     in.Id,
		Model:  in.Model,
		Output: output,
		Usage:  nativeUsage(in.Usage),
		Error: &responses2.Error{
			Type:    "",
			Message: "",
			Param:   "",
			Code:    "",
		},
		ServiceTier: in.ServiceTier,
		Metadata: map[string]any{
			"stop_reason":   in.StopReason,
			"stop_sequence": in.StopSequence,
		},
	}
}

// =============================================================================
// ResponseChunk to Native Conversion
// =============================================================================

// ResponseChunkToNativeResponseChunkConverter converts Anthropic stream chunks to native format.
// It maintains state across chunk conversions to accumulate deltas and track the current content block.
type ResponseChunkToNativeResponseChunkConverter struct {
	// State from message_start - contains message ID, model, role
	messageStart *ChunkMessage[ChunkTypeMessageStart]
	// State from message_delta - contains usage info
	messageDelta *ChunkMessage[ChunkTypeMessageDelta]
	// Current content block being processed
	currentBlock *ChunkContentBlock[ChunkTypeContentBlockStart]

	// Tracking
	sequenceNumber   int
	outputIndex      int
	contentIndex     int // Always 0, as each Anthropic content becomes a separate native message
	currentOutputID  string
	accumulatedDelta string
	accumulatedSig   string // Accumulated reasoning signature
	completedOutputs []responses2.OutputMessageUnion
}

// nextSeqNum returns the next sequence number and increments the counter.
func (c *ResponseChunkToNativeResponseChunkConverter) nextSeqNum() int {
	n := c.sequenceNumber
	c.sequenceNumber++
	return n
}

// currentRole returns the role from the message start, defaulting to assistant.
func (c *ResponseChunkToNativeResponseChunkConverter) currentRole() constants.Role {
	if c.messageStart != nil && c.messageStart.Message != nil {
		return c.messageStart.Message.Role.ToNativeRole()
	}
	return constants.RoleAssistant
}

// ResponseChunkToNativeResponseChunk converts a single Anthropic chunk to zero or more native chunks.
func (c *ResponseChunkToNativeResponseChunkConverter) ResponseChunkToNativeResponseChunk(in *ResponseChunk) []*responses2.ResponseChunk {
	if in == nil {
		return nil
	}

	switch {
	case in.OfMessageStart != nil:
		return c.handleMessageStart(in.OfMessageStart)
	case in.OfContentBlockStart != nil:
		return c.handleContentBlockStart(in.OfContentBlockStart)
	case in.OfContentBlockDelta != nil:
		return c.handleContentBlockDelta(in.OfContentBlockDelta)
	case in.OfContentBlockStop != nil:
		return c.handleContentBlockStop()
	case in.OfMessageDelta != nil:
		return c.handleMessageDelta(in.OfMessageDelta)
	case in.OfMessageStop != nil:
		return c.handleMessageStop()
	case in.OfPing != nil:
		return nil // Ping is keep-alive, no conversion needed
	}

	return nil
}

// =============================================================================
// Event Handlers
// =============================================================================

// handleMessageStart emits response.created and response.in_progress
func (c *ResponseChunkToNativeResponseChunkConverter) handleMessageStart(msg *ChunkMessage[ChunkTypeMessageStart]) []*responses2.ResponseChunk {
	c.messageStart = msg
	msgData := msg.Message

	return []*responses2.ResponseChunk{
		c.buildResponseCreated(msgData.Id, msgData.Model),
		c.buildResponseInProgress(msgData.Id),
	}
}

// handleContentBlockStart emits output_item.added (and content_part.added for text/reasoning)
func (c *ResponseChunkToNativeResponseChunkConverter) handleContentBlockStart(block *ChunkContentBlock[ChunkTypeContentBlockStart]) []*responses2.ResponseChunk {
	c.currentBlock = block
	content := block.ContentBlock

	switch {
	case content.OfText != nil:
		c.currentOutputID = responses2.NewOutputItemMessageID()
		return c.handleTextBlockStart()
	case content.OfToolUse != nil:
		c.currentOutputID = responses2.NewOutputItemFunctionCallID()
		return c.handleToolUseBlockStart(content.OfToolUse)
	case content.OfThinking != nil:
		c.currentOutputID = responses2.NewOutputItemReasoningID()
		return c.handleThinkingBlockStart(content.OfThinking)
	case content.OfServerToolUse != nil:
		c.currentOutputID = content.OfServerToolUse.Id
		return c.handleServerToolUseBlockStart(content.OfServerToolUse)
	case content.OfWebSearchResult != nil:
		return c.handleWebSearchToolResultBlockStart(content.OfWebSearchResult)
	case content.OfBashCodeExecutionToolResult != nil:
		return c.handleBashCodeExecutionToolResult(content.OfBashCodeExecutionToolResult)
	}

	return nil
}

func (c *ResponseChunkToNativeResponseChunkConverter) handleTextBlockStart() []*responses2.ResponseChunk {
	return []*responses2.ResponseChunk{
		c.buildOutputItemAddedMessage(),
		c.buildContentPartAddedText(),
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) handleToolUseBlockStart(toolUse *ToolUseContent) []*responses2.ResponseChunk {
	args, _ := sonic.Marshal(toolUse.Input)
	if args == nil {
		args = []byte("{}")
	}
	return []*responses2.ResponseChunk{
		c.buildOutputItemAddedFunctionCall(toolUse.ID, toolUse.Name, string(args)),
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) handleThinkingBlockStart(thinking *ThinkingContent) []*responses2.ResponseChunk {
	return []*responses2.ResponseChunk{
		c.buildOutputItemAddedReasoning(thinking.Signature),
		c.buildReasoningSummaryPartAdded(),
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) handleServerToolUseBlockStart(serverToolUse *ServerToolUseContent) []*responses2.ResponseChunk {
	if serverToolUse.Name == "web_search" {
		return []*responses2.ResponseChunk{
			c.buildOutputItemAddedWebSearchCall(),
			c.buildWebSearchCallInProgress(),
			c.buildWebSearchCallSearching(),
			c.buildWebSearchCallCompleted(),
		}
	}

	if serverToolUse.Name == "bash_code_execution" {
		return []*responses2.ResponseChunk{
			c.buildOutputItemAddedCodeInterpreterCall(serverToolUse.Input.Command),
			c.buildCodeInterpreterCallInProgress(),
		}
	}

	return []*responses2.ResponseChunk{}
}

func (c *ResponseChunkToNativeResponseChunkConverter) handleWebSearchToolResultBlockStart(webResultToolResult *WebSearchResultContent) []*responses2.ResponseChunk {
	return nil
}

func (c *ResponseChunkToNativeResponseChunkConverter) handleBashCodeExecutionToolResult(bashCodeExecutionToolResult *BashCodeExecutionResultContent) []*responses2.ResponseChunk {
	return nil
}

// handleContentBlockDelta emits delta chunks based on the current block type
func (c *ResponseChunkToNativeResponseChunkConverter) handleContentBlockDelta(delta *ChunkContentBlock[ChunkTypeContentBlockDelta]) []*responses2.ResponseChunk {
	if c.currentBlock == nil || c.currentBlock.ContentBlock == nil {
		return nil
	}

	content := c.currentBlock.ContentBlock

	switch {
	case content.OfText != nil && delta.Delta.OfText != nil:
		text := delta.Delta.OfText.Text
		c.accumulatedDelta += text
		return []*responses2.ResponseChunk{c.buildOutputTextDelta(text)}

	case content.OfText != nil && delta.Delta.OfCitation != nil:
		citation := delta.Delta.OfCitation.Citation
		return []*responses2.ResponseChunk{
			c.buildOutputTextAnnotationAdded(citation),
		}

	case content.OfToolUse != nil && delta.Delta.OfInputJSON != nil:
		json := delta.Delta.OfInputJSON.PartialJSON
		c.accumulatedDelta += json
		return []*responses2.ResponseChunk{c.buildFunctionCallArgumentsDelta(json)}

	case content.OfServerToolUse != nil && delta.Delta.OfInputJSON != nil:
		json := delta.Delta.OfInputJSON.PartialJSON
		c.accumulatedDelta += json

		if content.OfServerToolUse.Name == "bash_code_execution" {
			return []*responses2.ResponseChunk{
				c.buildCodeInterpreterCallCodeDelta(json),
			}
		}
		return nil

	case content.OfThinking != nil:
		return c.handleThinkingDelta(delta.Delta)
	}

	return nil
}

func (c *ResponseChunkToNativeResponseChunkConverter) handleThinkingDelta(delta *ChunkContentBlockDeltaUnion) []*responses2.ResponseChunk {
	if delta.OfThinking != nil {
		text := delta.OfThinking.Thinking
		c.accumulatedDelta += text
		return []*responses2.ResponseChunk{c.buildReasoningSummaryTextDelta(text)}
	}
	if delta.OfThinkingSignature != nil {
		sig := delta.OfThinkingSignature.Signature
		c.accumulatedSig += sig
		return []*responses2.ResponseChunk{c.buildReasoningSignatureDelta(sig)}
	}
	return nil
}

// handleContentBlockStop emits done chunks and stores the completed output
func (c *ResponseChunkToNativeResponseChunkConverter) handleContentBlockStop() []*responses2.ResponseChunk {
	if c.currentBlock == nil || c.currentBlock.ContentBlock == nil {
		return nil
	}

	var result []*responses2.ResponseChunk
	content := c.currentBlock.ContentBlock

	switch {
	case content.OfText != nil:
		result = c.completeTextBlock()
	case content.OfToolUse != nil:
		result = c.completeToolUseBlock(content.OfToolUse)
	case content.OfThinking != nil:
		result = c.completeThinkingBlock()
	case content.OfServerToolUse != nil:
		return c.completeServerToolUseBlock(content.OfServerToolUse)
	case content.OfWebSearchResult != nil:
		result = c.completeWebSearchCallBlock(content.OfWebSearchResult)
	case content.OfBashCodeExecutionToolResult != nil:
		result = c.completeBashCodeExecutionToolResult(content.OfBashCodeExecutionToolResult)
	}

	// Reset for next block
	c.outputIndex++
	c.accumulatedDelta = ""
	c.accumulatedSig = ""

	return result
}

func (c *ResponseChunkToNativeResponseChunkConverter) completeTextBlock() []*responses2.ResponseChunk {
	text := c.accumulatedDelta
	role := c.currentRole()

	// Store for final response
	c.completedOutputs = append(c.completedOutputs, responses2.OutputMessageUnion{
		OfOutputMessage: &responses2.OutputMessage{
			ID:   c.currentOutputID,
			Role: role,
			Content: &responses2.OutputContent{
				{OfOutputText: &responses2.OutputTextContent{Text: text}},
			},
		},
	})

	return []*responses2.ResponseChunk{
		c.buildOutputTextDone(text),
		c.buildContentPartDoneText(text),
		c.buildOutputItemDoneMessage(text, role),
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) completeToolUseBlock(toolUse *ToolUseContent) []*responses2.ResponseChunk {
	args := c.accumulatedDelta
	if args == "" {
		args = "{}"
	}

	// Store for final response
	c.completedOutputs = append(c.completedOutputs, responses2.OutputMessageUnion{
		OfFunctionCall: &responses2.FunctionCallMessage{
			ID:        c.currentOutputID,
			CallID:    toolUse.ID,
			Name:      toolUse.Name,
			Arguments: args,
		},
	})

	return []*responses2.ResponseChunk{
		c.buildFunctionCallArgumentsDone(args),
		c.buildOutputItemDoneFunctionCall(toolUse.ID, toolUse.Name, args),
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) completeThinkingBlock() []*responses2.ResponseChunk {
	text := c.accumulatedDelta
	sig := c.accumulatedSig

	// Store for final response
	c.completedOutputs = append(c.completedOutputs, responses2.OutputMessageUnion{
		OfReasoning: &responses2.ReasoningMessage{
			ID:               c.currentOutputID,
			Summary:          []responses2.SummaryTextContent{{Text: text}},
			EncryptedContent: utils.Ptr(sig),
		},
	})

	return []*responses2.ResponseChunk{
		c.buildReasoningSummaryTextDone(text),
		c.buildReasoningSummaryPartDone(text),
		c.buildOutputItemDoneReasoning(text, sig),
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) completeServerToolUseBlock(serverToolUse *ServerToolUseContent) []*responses2.ResponseChunk {
	text := c.accumulatedDelta

	if serverToolUse.Name == "web_search" {
		return nil // we don't do anything for server_tool_use content stop, we will wait for content_block_stop of "web_search_tool_result"
	}

	if serverToolUse.Name == "bash_code_execution" {
		return []*responses2.ResponseChunk{
			c.buildCodeInterpreterCallCodeDone(text),
			c.buildCodeInterpreterCallInterpreting(),
		}
	}

	return nil
}

func (c *ResponseChunkToNativeResponseChunkConverter) completeWebSearchCallBlock(webSearchResult *WebSearchResultContent) []*responses2.ResponseChunk {
	return []*responses2.ResponseChunk{
		c.buildOutputItemDoneWebSearchCall(webSearchResult),
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) completeBashCodeExecutionToolResult(bashCodeExecutionResult *BashCodeExecutionResultContent) []*responses2.ResponseChunk {
	text := c.accumulatedDelta

	output := c.currentBlock.ContentBlock.OfBashCodeExecutionToolResult.Content.Stdout
	if c.currentBlock.ContentBlock.OfBashCodeExecutionToolResult.Content.ReturnCode > 0 {
		output = c.currentBlock.ContentBlock.OfBashCodeExecutionToolResult.Content.Stderr
	}

	return []*responses2.ResponseChunk{
		c.buildCodeInterpreterCallCompleted(text),
		c.buildOutputItemDoneCodeInterpreterCall(text, output),
	}
}

// handleMessageDelta stores usage info for the final response
func (c *ResponseChunkToNativeResponseChunkConverter) handleMessageDelta(delta *ChunkMessage[ChunkTypeMessageDelta]) []*responses2.ResponseChunk {
	c.messageDelta = delta
	return nil
}

// handleMessageStop emits response.completed
func (c *ResponseChunkToNativeResponseChunkConverter) handleMessageStop() []*responses2.ResponseChunk {
	return []*responses2.ResponseChunk{c.buildResponseCompleted()}
}

// =============================================================================
// Chunk Builders
// =============================================================================

func (c *ResponseChunkToNativeResponseChunkConverter) buildResponseCreated(id, model string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfResponseCreated: &responses2.ChunkResponse[constants.ChunkTypeResponseCreated]{
			Type:           constants.ChunkTypeResponseCreated(""),
			SequenceNumber: c.nextSeqNum(),
			Response: responses2.ChunkResponseData{
				Id:         id,
				Object:     "response",
				CreatedAt:  int(time.Now().Unix()),
				Status:     "in_progress",
				Background: false,
				Request:    responses2.Request{Model: model},
			},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildResponseInProgress(id string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfResponseInProgress: &responses2.ChunkResponse[constants.ChunkTypeResponseInProgress]{
			Type:           constants.ChunkTypeResponseInProgress(""),
			SequenceNumber: c.nextSeqNum(),
			Response: responses2.ChunkResponseData{
				Id:         id,
				Object:     "response",
				CreatedAt:  int(time.Now().Unix()),
				Status:     "in_progress",
				Background: false,
			},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildOutputItemAddedMessage() *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfOutputItemAdded: &responses2.ChunkOutputItem[constants.ChunkTypeOutputItemAdded]{
			Type:           constants.ChunkTypeOutputItemAdded(""),
			SequenceNumber: c.nextSeqNum(),
			OutputIndex:    c.outputIndex,
			Item: responses2.ChunkOutputItemData{
				Type:    "message",
				Id:      c.currentOutputID,
				Status:  "in_progress",
				Role:    c.currentRole(),
				Content: &responses2.ChunkOutputItemContent{},
			},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildOutputItemAddedFunctionCall(callID, name, args string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfOutputItemAdded: &responses2.ChunkOutputItem[constants.ChunkTypeOutputItemAdded]{
			Type:           constants.ChunkTypeOutputItemAdded(""),
			SequenceNumber: c.nextSeqNum(),
			OutputIndex:    c.outputIndex,
			Item: responses2.ChunkOutputItemData{
				Type:      "function_call",
				Id:        c.currentOutputID,
				Status:    "in_progress",
				CallID:    utils.Ptr(callID),
				Name:      utils.Ptr(name),
				Arguments: utils.Ptr(args),
			},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildOutputItemAddedReasoning(signature string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfOutputItemAdded: &responses2.ChunkOutputItem[constants.ChunkTypeOutputItemAdded]{
			Type:           constants.ChunkTypeOutputItemAdded(""),
			SequenceNumber: c.nextSeqNum(),
			OutputIndex:    c.outputIndex,
			Item: responses2.ChunkOutputItemData{
				Type:             "reasoning",
				Id:               c.currentOutputID,
				Status:           "in_progress",
				Summary:          &[]responses2.SummaryTextContent{},
				EncryptedContent: utils.Ptr(signature),
			},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildOutputItemAddedWebSearchCall() *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfOutputItemAdded: &responses2.ChunkOutputItem[constants.ChunkTypeOutputItemAdded]{
			Type:           constants.ChunkTypeOutputItemAdded(""),
			SequenceNumber: c.nextSeqNum(),
			OutputIndex:    c.outputIndex,
			Item: responses2.ChunkOutputItemData{
				Type:   "web_search_call",
				Id:     c.currentOutputID,
				Status: "in_progress",
				Action: &responses2.WebSearchCallActionUnion{
					OfSearch: &responses2.WebSearchCallActionOfSearch{},
				},
			},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildWebSearchCallInProgress() *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfWebSearchCallInProgress: &responses2.ChunkWebSearchCall[constants.ChunkTypeWebSearchCallInProgress]{
			Type:           constants.ChunkTypeWebSearchCallInProgress(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.currentOutputID,
			OutputIndex:    c.outputIndex,
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildWebSearchCallSearching() *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfWebSearchCallSearching: &responses2.ChunkWebSearchCall[constants.ChunkTypeWebSearchCallSearching]{
			Type:           constants.ChunkTypeWebSearchCallSearching(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.currentOutputID,
			OutputIndex:    c.outputIndex,
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildWebSearchCallCompleted() *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfWebSearchCallCompleted: &responses2.ChunkWebSearchCall[constants.ChunkTypeWebSearchCallCompleted]{
			Type:           constants.ChunkTypeWebSearchCallCompleted(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.currentOutputID,
			OutputIndex:    c.outputIndex,
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildOutputItemAddedCodeInterpreterCall(code string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfOutputItemAdded: &responses2.ChunkOutputItem[constants.ChunkTypeOutputItemAdded]{
			Type:           constants.ChunkTypeOutputItemAdded(""),
			SequenceNumber: c.nextSeqNum(),
			OutputIndex:    c.outputIndex,
			Item: responses2.ChunkOutputItemData{
				Type:   "code_interpreter_call",
				Id:     c.currentOutputID,
				Status: "in_progress",
				Code:   &code,
			},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildCodeInterpreterCallInProgress() *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfCodeInterpreterCallInProgress: &responses2.ChunkCodeInterpreterCall[constants.ChunkTypeCodeInterpreterCallInProgress]{
			Type:           constants.ChunkTypeCodeInterpreterCallInProgress(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.currentOutputID,
			OutputIndex:    c.outputIndex,
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildCodeInterpreterCallCodeDelta(delta string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfCodeInterpreterCallCodeDelta: &responses2.ChunkCodeInterpreterCall[constants.ChunkTypeCodeInterpreterCallCodeDelta]{
			Type:           constants.ChunkTypeCodeInterpreterCallCodeDelta(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.currentOutputID,
			OutputIndex:    c.outputIndex,
			Delta:          &delta,
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildCodeInterpreterCallCodeDone(code string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfCodeInterpreterCallCodeDone: &responses2.ChunkCodeInterpreterCall[constants.ChunkTypeCodeInterpreterCallCodeDone]{
			Type:           constants.ChunkTypeCodeInterpreterCallCodeDone(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.currentOutputID,
			OutputIndex:    c.outputIndex,
			Code:           &code,
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildCodeInterpreterCallInterpreting() *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfCodeInterpreterCallInterpreting: &responses2.ChunkCodeInterpreterCall[constants.ChunkTypeCodeInterpreterCallInterpreting]{
			Type:           constants.ChunkTypeCodeInterpreterCallInterpreting(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.currentOutputID,
			OutputIndex:    c.outputIndex,
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildCodeInterpreterCallCompleted(code string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfCodeInterpreterCallCompleted: &responses2.ChunkCodeInterpreterCall[constants.ChunkTypeCodeInterpreterCallCompleted]{
			Type:           constants.ChunkTypeCodeInterpreterCallCompleted(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.currentOutputID,
			OutputIndex:    c.outputIndex,
			Code:           &code,
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildOutputItemDoneCodeInterpreterCall(code string, output string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfOutputItemDone: &responses2.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{
			Type:           constants.ChunkTypeOutputItemDone(""),
			SequenceNumber: c.nextSeqNum(),
			OutputIndex:    c.outputIndex,
			Item: responses2.ChunkOutputItemData{
				Type:   "code_interpreter_call",
				Id:     c.currentOutputID,
				Status: "completed",
				Code:   &code,
				Outputs: []responses2.CodeInterpreterCallOutputParam{
					{
						Type: "logs",
						Logs: output,
					},
				},
			},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildContentPartAddedText() *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfContentPartAdded: &responses2.ChunkContentPart[constants.ChunkTypeContentPartAdded]{
			Type:           constants.ChunkTypeContentPartAdded(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.currentOutputID,
			OutputIndex:    c.outputIndex,
			ContentIndex:   c.contentIndex,
			Part:           responses2.ChunkOutputItemContentUnion{OfOutputText: &responses2.OutputTextContent{Text: ""}},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildReasoningSummaryPartAdded() *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfReasoningSummaryPartAdded: &responses2.ChunkReasoningSummaryPart[constants.ChunkTypeReasoningSummaryPartAdded]{
			Type:           constants.ChunkTypeReasoningSummaryPartAdded(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.currentOutputID,
			OutputIndex:    c.outputIndex,
			SummaryIndex:   c.contentIndex,
			Part:           responses2.SummaryTextContent{Text: ""},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildOutputTextDelta(delta string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfOutputTextDelta: &responses2.ChunkOutputText[constants.ChunkTypeOutputTextDelta]{
			Type:           constants.ChunkTypeOutputTextDelta(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.currentOutputID,
			OutputIndex:    c.outputIndex,
			ContentIndex:   c.contentIndex,
			Delta:          delta,
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildOutputTextAnnotationAdded(citation Citation) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfOutputTextAnnotationAdded: &responses2.ChunkOutputText[constants.ChunkTypeOutputTextAnnotationAdded]{
			Type:           constants.ChunkTypeOutputTextAnnotationAdded(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.currentOutputID,
			OutputIndex:    c.outputIndex,
			ContentIndex:   c.contentIndex,
			Annotation: &responses2.Annotation{
				Type:       "url_citation",
				Title:      citation.Title,
				URL:        citation.Url,
				StartIndex: 0,
				EndIndex:   0,
				ExtraParams: map[string]any{
					"Anthropic": citation,
				},
			},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildFunctionCallArgumentsDelta(delta string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfFunctionCallArgumentsDelta: &responses2.ChunkFunctionCall[constants.ChunkTypeFunctionCallArgumentsDelta]{
			Type:           constants.ChunkTypeFunctionCallArgumentsDelta(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.currentOutputID,
			OutputIndex:    c.outputIndex,
			Delta:          delta,
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildReasoningSummaryTextDelta(delta string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfReasoningSummaryTextDelta: &responses2.ChunkReasoningSummaryText[constants.ChunkTypeReasoningSummaryTextDelta]{
			Type:           constants.ChunkTypeReasoningSummaryTextDelta(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.currentOutputID,
			OutputIndex:    c.outputIndex,
			SummaryIndex:   c.contentIndex,
			Delta:          delta,
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildReasoningSignatureDelta(sig string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfReasoningSummaryTextDelta: &responses2.ChunkReasoningSummaryText[constants.ChunkTypeReasoningSummaryTextDelta]{
			Type:             constants.ChunkTypeReasoningSummaryTextDelta(""),
			SequenceNumber:   c.nextSeqNum(),
			ItemId:           c.currentOutputID,
			OutputIndex:      c.outputIndex,
			SummaryIndex:     c.contentIndex,
			EncryptedContent: utils.Ptr(sig),
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildOutputTextDone(text string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfOutputTextDone: &responses2.ChunkOutputText[constants.ChunkTypeOutputTextDone]{
			Type:           constants.ChunkTypeOutputTextDone(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.currentOutputID,
			OutputIndex:    c.outputIndex,
			ContentIndex:   c.contentIndex,
			Text:           utils.Ptr(text),
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildContentPartDoneText(text string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfContentPartDone: &responses2.ChunkContentPart[constants.ChunkTypeContentPartDone]{
			Type:           constants.ChunkTypeContentPartDone(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.currentOutputID,
			OutputIndex:    c.outputIndex,
			ContentIndex:   c.contentIndex,
			Part:           responses2.ChunkOutputItemContentUnion{OfOutputText: &responses2.OutputTextContent{Text: text}},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildOutputItemDoneMessage(text string, role constants.Role) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfOutputItemDone: &responses2.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{
			Type:           constants.ChunkTypeOutputItemDone(""),
			SequenceNumber: c.nextSeqNum(),
			OutputIndex:    c.outputIndex,
			Item: responses2.ChunkOutputItemData{
				Type:    "message",
				Id:      c.currentOutputID,
				Status:  "completed",
				Role:    role,
				Content: &responses2.ChunkOutputItemContent{{OfOutputText: &responses2.OutputTextContent{Text: text}}},
			},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildFunctionCallArgumentsDone(args string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfFunctionCallArgumentsDone: &responses2.ChunkFunctionCall[constants.ChunkTypeFunctionCallArgumentsDone]{
			Type:           constants.ChunkTypeFunctionCallArgumentsDone(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.currentOutputID,
			OutputIndex:    c.outputIndex,
			Arguments:      args,
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildOutputItemDoneFunctionCall(callID, name, args string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfOutputItemDone: &responses2.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{
			Type:           constants.ChunkTypeOutputItemDone(""),
			SequenceNumber: c.nextSeqNum(),
			OutputIndex:    c.outputIndex,
			Item: responses2.ChunkOutputItemData{
				Type:      "function_call",
				Id:        c.currentOutputID,
				Status:    "completed",
				CallID:    utils.Ptr(callID),
				Name:      utils.Ptr(name),
				Arguments: utils.Ptr(args),
			},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildReasoningSummaryTextDone(text string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfReasoningSummaryTextDone: &responses2.ChunkReasoningSummaryText[constants.ChunkTypeReasoningSummaryTextDone]{
			Type:           constants.ChunkTypeReasoningSummaryTextDone(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.currentOutputID,
			OutputIndex:    c.outputIndex,
			SummaryIndex:   c.contentIndex,
			Text:           utils.Ptr(text),
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildReasoningSummaryPartDone(text string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfReasoningSummaryPartDone: &responses2.ChunkReasoningSummaryPart[constants.ChunkTypeReasoningSummaryPartDone]{
			Type:           constants.ChunkTypeReasoningSummaryPartDone(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.currentOutputID,
			OutputIndex:    c.outputIndex,
			SummaryIndex:   c.contentIndex,
			Part:           responses2.SummaryTextContent{Text: text},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildOutputItemDoneReasoning(text, sig string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfOutputItemDone: &responses2.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{
			Type:           constants.ChunkTypeOutputItemDone(""),
			SequenceNumber: c.nextSeqNum(),
			OutputIndex:    c.outputIndex,
			Item: responses2.ChunkOutputItemData{
				Type:             "reasoning",
				Id:               c.currentOutputID,
				Status:           "completed",
				EncryptedContent: utils.Ptr(sig),
				Summary:          &[]responses2.SummaryTextContent{{Text: text}},
			},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildOutputItemDoneWebSearchCall(webSearchResults *WebSearchResultContent) *responses2.ResponseChunk {
	sources := []responses2.WebSearchCallActionOfSearchSource{}
	for _, webSearchResult := range webSearchResults.Content {
		sources = append(sources, responses2.WebSearchCallActionOfSearchSource{
			Type: "url",
			URL:  webSearchResult.Url,
			ExtraParams: map[string]any{
				"Anthropic": webSearchResult,
			},
		})
	}

	query := ""
	accumulatedPayload := struct {
		Query string `json:"query"`
	}{}
	if err := sonic.Unmarshal([]byte(c.accumulatedDelta), &accumulatedPayload); err == nil {
		query = accumulatedPayload.Query
	}

	return &responses2.ResponseChunk{
		OfOutputItemDone: &responses2.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{
			Type:           constants.ChunkTypeOutputItemDone(""),
			SequenceNumber: c.nextSeqNum(),
			OutputIndex:    0,
			Item: responses2.ChunkOutputItemData{
				Type:   "web_search_call",
				Id:     c.currentOutputID,
				Status: "completed",
				Action: &responses2.WebSearchCallActionUnion{
					OfSearch: &responses2.WebSearchCallActionOfSearch{
						Queries: []string{query},
						Query:   query,
						Sources: sources,
					},
				},
			},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildResponseCompleted() *responses2.ResponseChunk {
	var msgID, model string
	if c.messageStart != nil && c.messageStart.Message != nil {
		msgID = c.messageStart.Message.Id
		model = c.messageStart.Message.Model
	}

	var usage *responses2.Usage
	if u := c.streamUsage(); u != nil {
		usage = u
	} else {
		usage = &responses2.Usage{}
	}

	return &responses2.ResponseChunk{
		OfResponseCompleted: &responses2.ChunkResponse[constants.ChunkTypeResponseCompleted]{
			Type:           constants.ChunkTypeResponseCompleted(""),
			SequenceNumber: c.nextSeqNum(),
			Response: responses2.ChunkResponseData{
				Id:        msgID,
				Object:    "response",
				CreatedAt: int(time.Now().Unix()),
				Status:    "completed",
				Output:    c.completedOutputs,
				Usage:     *usage,
				Request:   responses2.Request{Model: model},
			},
		},
	}
}

// streamUsage reconciles the two events that carry usage in an Anthropic
// stream. message_start reports the prompt counts (input_tokens plus the
// cache_creation/cache_read splits) up front and leaves output_tokens at an
// initial placeholder; message_delta reports the final output_tokens once
// generation ends.
//
// Reading message_delta alone — as this did — is wrong for the shape the agent
// loop actually produces: on a plain text or tool-use turn that event carries
// *only* output_tokens, so the entire prompt was missing from the accounting
// and ContextTokens was really just the output count. Some turns (server tool
// use) do echo the full prompt counts on message_delta, and some (thinking)
// omit usage from both events entirely — so neither event can be treated as
// the sole source.
//
// Take the larger of the two for every field: whichever event carries a real
// value wins, and an event that repeats it is idempotent. message_delta's
// counts are documented as cumulative rather than incremental, so max is the
// right combiner — summing would double-count the echoed turns.
func (c *ResponseChunkToNativeResponseChunkConverter) streamUsage() *responses2.Usage {
	var start, delta *ChunkMessageUsage
	if c.messageStart != nil && c.messageStart.Message != nil {
		start = c.messageStart.Message.Usage
	}
	if c.messageDelta != nil {
		delta = c.messageDelta.Usage
	}

	if start == nil && delta == nil {
		return nil
	}

	merged := ChunkMessageUsage{}
	for _, u := range []*ChunkMessageUsage{start, delta} {
		if u == nil {
			continue
		}
		merged.InputTokens = max(merged.InputTokens, u.InputTokens)
		merged.CacheCreationInputTokens = max(merged.CacheCreationInputTokens, u.CacheCreationInputTokens)
		merged.CacheReadInputTokens = max(merged.CacheReadInputTokens, u.CacheReadInputTokens)
		merged.OutputTokens = max(merged.OutputTokens, u.OutputTokens)
	}

	return nativeUsage(&merged)
}

// nativeUsage normalizes Anthropic's token accounting onto the native Usage
// contract. Anthropic reports input_tokens as only the portion of the prompt
// that was neither read from nor written to the cache, with
// cache_read_input_tokens and cache_creation_input_tokens counted separately —
// so the real prompt size is the sum of all three.
func nativeUsage(u *ChunkMessageUsage) *responses2.Usage {
	if u == nil {
		return nil
	}

	out := UsageWithSplitCacheTokens(
		u.InputTokens,
		u.CacheReadInputTokens,
		u.CacheCreationInputTokens,
		u.OutputTokens,
		0,
	)

	return &out
}

// UsageWithSplitCacheTokens normalizes usage from a provider that reports
// cache tokens *alongside* its prompt token count rather than inside it.
// Anthropic is the case in point: input_tokens counts only what was neither
// read from nor written to the cache, so the real prompt size is the sum of
// all three fields. Both cache counts are prompt tokens the model processed,
// so both fold into InputTokens; only the read portion is "cached" for
// reporting, since a cache write did the full work of processing those tokens.
func UsageWithSplitCacheTokens(uncachedPrompt, cacheRead, cacheWrite, output, reasoning int) responses2.Usage {
	input := uncachedPrompt + cacheRead + cacheWrite

	u := responses2.Usage{
		InputTokens:  input,
		OutputTokens: output,
		TotalTokens:  input + output,
	}
	u.InputTokensDetails.CachedTokens = cacheRead
	u.OutputTokensDetails.ReasoningTokens = reasoning

	return u
}
