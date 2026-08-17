package anthropic_responses

import (
	"log/slog"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	responses2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

func NativeRequestToRequest(in *responses2.Request) *Request {
	if in.MaxOutputTokens == nil {
		in.MaxOutputTokens = utils.Ptr(512)
	}

	if in.MaxToolCalls != nil {
		slog.Warn("max tool call is not supported for anthropic models")
	}

	if in.ParallelToolCalls != nil {
		slog.Warn("parallel tool call is not supported for anthropic models")
	}

	out := &Request{
		Temperature: in.Temperature,
		MaxTokens:   *in.MaxOutputTokens,
		TopP:        in.TopP,
		TopK:        in.TopLogprobs,
		Model:       in.Model,
		Messages:    NativeMessagesToMessage(in.Input),
		Metadata:    in.Metadata,
		Tools:       NativeToolsToTools(in.Tools),
		Stream:      in.Stream,
	}

	if in.Instructions != nil && *in.Instructions != "" {
		out.System = []TextContent{
			{
				Text: *in.Instructions,
			},
		}
	}

	if in.Reasoning != nil {
		if IsAdaptiveThinkingModel(in.Model) {
			out.Thinking = &ThinkingParam{
				Type: utils.Ptr("adaptive"),
			}

			if out.OutputConfig == nil {
				out.OutputConfig = &OutputConfig{}
			}

			switch *in.Reasoning.Effort {
			case "none":
				out.Thinking.Type = utils.Ptr("disabled")
			case "low":
				out.OutputConfig.Effort = utils.Ptr("low")
			case "medium":
				out.OutputConfig.Effort = utils.Ptr("medium")
			case "high":
				out.OutputConfig.Effort = utils.Ptr("high")
			case "xhigh":
				out.OutputConfig.Effort = utils.Ptr("xhigh")
			default:
				out.OutputConfig.Effort = utils.Ptr("high")
			}

		} else {
			out.Thinking = &ThinkingParam{
				Type: utils.Ptr("enabled"),
			}

			// Budget tokens (derived from effort)
			switch *in.Reasoning.Effort {
			case "none":
				out.Thinking.Type = utils.Ptr("disabled")
			case "low":
				out.Thinking.BudgetTokens = utils.Ptr(1024)
			case "medium":
				out.Thinking.BudgetTokens = utils.Ptr(3000)
			case "high":
				out.Thinking.BudgetTokens = utils.Ptr(6000)
			case "xhigh":
				out.Thinking.BudgetTokens = utils.Ptr(10000)
			}
		}
	}

	if in.Text != nil {
		out.OutputFormat = in.Text.Format

		// Since anthropic doesn't allow extra keys, we delete keys that are specific to openai
		delete(out.OutputFormat, "name")
		delete(out.OutputFormat, "strict")
	}

	if in.ExtraFields != nil {
		buf, err := sonic.Marshal(in.ExtraFields)
		if err != nil {
			slog.Error("error in marshaling extra fields")
			return nil
		}

		var extraFields ExtraFields
		err = sonic.Unmarshal(buf, &extraFields)
		if err != nil {
			slog.Error("error in unmarshaling extra fields", slog.Any("err", err.Error()))
			return nil
		}

		out.ExtraFields = extraFields
	}

	return out
}

func NativeRoleToRole(in constants.Role) Role {
	switch in {
	case constants.RoleUser:
		return RoleUser
	case constants.RoleSystem, constants.RoleDeveloper:
		return RoleUser
	case constants.RoleAssistant:
		return RoleAssistant
	}

	return RoleAssistant
}

func NativeToolsToTools(nativeTools []responses2.ToolUnion) []ToolUnion {
	out := []ToolUnion{}

	for _, nativeTool := range nativeTools {
		if nativeTool.OfFunction != nil {
			out = append(out, ToolUnion{
				OfCustomTool: &CustomTool{
					Type:        "custom",
					Name:        nativeTool.OfFunction.Name,
					Description: nativeTool.OfFunction.Description,
					InputSchema: nativeTool.OfFunction.Parameters,
				},
			})
		}

		if nativeTool.OfWebSearch != nil {
			webSearchTool := &WebSearchTool{
				Type:           "web_search_20250305",
				Name:           "web_search",
				MaxUses:        5,
				BlockedDomains: nil,
			}

			if nativeTool.OfWebSearch.Filters != nil {
				if nativeTool.OfWebSearch.Filters.AllowedDomains != nil {
					webSearchTool.AllowedDomains = nativeTool.OfWebSearch.Filters.AllowedDomains
				}
			}

			if nativeTool.OfWebSearch.UserLocation != nil {
				webSearchTool.UserLocation = &WebSearchToolUserLocationParam{
					Type:     nativeTool.OfWebSearch.UserLocation.Type,
					City:     nativeTool.OfWebSearch.UserLocation.City,
					Region:   nativeTool.OfWebSearch.UserLocation.Region,
					Country:  nativeTool.OfWebSearch.UserLocation.Country,
					Timezone: nativeTool.OfWebSearch.UserLocation.Timezone,
				}
			}

			out = append(out, ToolUnion{
				OfWebSearchTool: webSearchTool,
			})
		}

		if nativeTool.OfCodeExecution != nil {
			codeExecutionTool := &CodeExecutionTool{
				Type: "code_execution_20250825",
				Name: "code_execution",
			}

			out = append(out, ToolUnion{
				OfCodeExecutionTool: codeExecutionTool,
			})
		}
	}

	return out
}

func NativeAnnotationsToCitations(annotations []responses2.Annotation) []Citation {
	var citations []Citation
	for _, annotation := range annotations {
		if raw, exists := annotation.ExtraParams["Anthropic"]; exists {
			if anthropicCitation, ok := raw.(Citation); ok {
				citations = append(citations, anthropicCitation)
			}
		} else {
			citations = append(citations, Citation{
				Type:           "web_search_result_location",
				Url:            annotation.URL,
				Title:          annotation.Title,
				EncryptedIndex: "",
				CitedText:      "",
			})
		}
	}
	return citations
}

func NativeMessagesToMessage(in responses2.InputUnion) []MessageUnion {
	out := []MessageUnion{}

	if in.OfString != nil {
		out = append(out, MessageUnion{
			Role: RoleUser,
			Content: ContentUnionParam{
				OfList: Contents{
					ContentUnion{
						OfText: &TextContent{
							Text: *in.OfString,
						},
					},
				},
			},
		})
		return out
	}

	if in.OfInputMessageList != nil {
		for _, nativeMessage := range in.OfInputMessageList {
			if nativeMessage.OfEasyInput != nil {
				contents := Contents{}

				if nativeMessage.OfEasyInput.Content.OfString != nil {
					contents = append(contents, ContentUnion{OfText: &TextContent{
						Text: *nativeMessage.OfEasyInput.Content.OfString,
					}})
				}

				if nativeMessage.OfEasyInput.Content.OfInputMessageList != nil {
					for _, nativeContent := range nativeMessage.OfEasyInput.Content.OfInputMessageList {
						if nativeContent.OfInputText != nil {
							contents = append(contents, ContentUnion{
								OfText: &TextContent{
									Text: nativeContent.OfInputText.Text,
								},
							})
						}

						if nativeContent.OfOutputText != nil {
							contents = append(contents, ContentUnion{
								OfText: &TextContent{
									Text:      nativeContent.OfOutputText.Text,
									Citations: NativeAnnotationsToCitations(nativeContent.OfOutputText.Annotations),
								},
							})
						}

						if nativeContent.OfInputImage != nil {
							// Base64
							if strings.HasPrefix(*nativeContent.OfInputImage.ImageURL, "data:") {
								contentType, data, err := utils.ParseDataURL(*nativeContent.OfInputImage.ImageURL)
								if err != nil {
									slog.Warn("error in parsing data url")
									continue
								}

								contents = append(contents, ContentUnion{
									OfImage: &ImageContent{
										Source: ImageContentSource{
											Type:      "base64",
											Data:      utils.Ptr(data),
											MediaType: utils.Ptr(contentType),
										},
									},
								})
							}

							// TODO: URL & File
						}
					}
				}

				out = append(out, MessageUnion{
					Role:    NativeRoleToRole(nativeMessage.OfEasyInput.Role),
					Content: ContentUnionParam{OfList: contents},
				})
			}

			if nativeMessage.OfInputMessage != nil {
				contents := Contents{}

				for _, nativeContent := range nativeMessage.OfInputMessage.Content {
					if nativeContent.OfInputText != nil {
						contents = append(contents, ContentUnion{
							OfText: &TextContent{
								Text: nativeContent.OfInputText.Text,
							},
						})
					}

					if nativeContent.OfOutputText != nil {
						contents = append(contents, ContentUnion{
							OfText: &TextContent{
								Text:      nativeContent.OfOutputText.Text,
								Citations: NativeAnnotationsToCitations(nativeContent.OfOutputText.Annotations),
							},
						})
					}

					if nativeContent.OfInputImage != nil {
						// Base64
						if strings.HasPrefix(*nativeContent.OfInputImage.ImageURL, "data:") {
							contentType, data, err := utils.ParseDataURL(*nativeContent.OfInputImage.ImageURL)
							if err != nil {
								slog.Warn("error in parsing data url")
								continue
							}

							contents = append(contents, ContentUnion{
								OfImage: &ImageContent{
									Source: ImageContentSource{
										Type:      "base64",
										Data:      utils.Ptr(data),
										MediaType: utils.Ptr(contentType),
									},
								},
							})
						}

						// TODO: URL & File
					}
				}

				out = append(out, MessageUnion{
					Role:    NativeRoleToRole(nativeMessage.OfInputMessage.Role),
					Content: ContentUnionParam{OfList: contents},
				})
			}

			if nativeMessage.OfFunctionCall != nil {
				args := map[string]any{}
				if err := sonic.Unmarshal([]byte(nativeMessage.OfFunctionCall.Arguments), &args); err != nil {
					slog.Warn("unable to unmarshal tool_use args - string into map[string]any")
				}

				out = append(out, MessageUnion{
					Role: RoleAssistant,
					Content: ContentUnionParam{
						OfList: Contents{
							{
								OfToolUse: &ToolUseContent{
									ID:    nativeMessage.OfFunctionCall.CallID,
									Name:  nativeMessage.OfFunctionCall.Name,
									Input: args,
								},
							},
						},
					},
				})
			}

			if nativeMessage.OfFunctionCallOutput != nil {
				output := Contents{}

				if nativeMessage.OfFunctionCallOutput.Output.OfString != nil {
					output = append(output, ContentUnion{
						OfText: &TextContent{
							Text: *nativeMessage.OfFunctionCallOutput.Output.OfString,
						},
					})
				}

				if nativeMessage.OfFunctionCallOutput.Output.OfList != nil {
					for _, nativeOutput := range nativeMessage.OfFunctionCallOutput.Output.OfList {
						if nativeOutput.OfInputText != nil {
							output = append(output, ContentUnion{
								OfText: &TextContent{
									Text: nativeOutput.OfInputText.Text,
								},
							})
						}
					}
				}

				out = append(out, MessageUnion{
					Role: RoleUser,
					Content: ContentUnionParam{OfList: Contents{
						{
							OfToolResult: &ToolUseResultContent{
								ToolUseID: nativeMessage.OfFunctionCallOutput.CallID,
								Content:   ContentUnionParam{OfList: output},
								IsError:   nil,
							},
						},
					}},
				})
			}

			// Reasoning can be thinking or redacted_thinking
			if nativeMessage.OfReasoning != nil {
				if nativeMessage.OfReasoning.EncryptedContent == nil || *nativeMessage.OfReasoning.EncryptedContent == "" {
					continue
				}

				// Thinking
				if nativeMessage.OfReasoning.Summary != nil {
					thinking := ""
					for _, nativeThinkingContent := range nativeMessage.OfReasoning.Summary {
						thinking += nativeThinkingContent.Text
					}

					out = append(out, MessageUnion{
						Role: RoleAssistant,
						Content: ContentUnionParam{
							OfList: Contents{
								{
									OfThinking: &ThinkingContent{
										Thinking:  thinking,
										Signature: *nativeMessage.OfReasoning.EncryptedContent,
									},
								},
							},
						},
					})
				}

				// Redacted Thinking
				if nativeMessage.OfReasoning.Summary == nil {
					out = append(out, MessageUnion{
						Role: RoleAssistant,
						Content: ContentUnionParam{
							OfList: Contents{
								{
									OfThinking: &ThinkingContent{
										Signature: *nativeMessage.OfReasoning.EncryptedContent,
									},
								},
							},
						},
					})
				}
			}

			if nativeMessage.OfWebSearchCall != nil {
				if nativeMessage.OfWebSearchCall.Action.OfSearch != nil {
					// If the native message contains "sources" (obtained by include=["web_search_call.action.sources"]), then we will
					// map it to server_tool_use and web_search_tool_result
					// otherwise we will skip it
					if nativeMessage.OfWebSearchCall.Action.OfSearch.Sources != nil && len(nativeMessage.OfWebSearchCall.Action.OfSearch.Sources) > 0 {
						query := nativeMessage.OfWebSearchCall.Action.OfSearch.Query

						var contents Contents

						// server_tool_use
						contents = append(contents, ContentUnion{
							OfServerToolUse: &ServerToolUseContent{
								Id:   nativeMessage.OfWebSearchCall.ID,
								Name: "web_search",
								Input: struct {
									Query   string `json:"query"`
									Command string `json:"command"`
								}{
									Query: query,
								},
							},
						})

						results := []WebSearchResultContentParam{}
						for _, source := range nativeMessage.OfWebSearchCall.Action.OfSearch.Sources {
							raw, exists := source.ExtraParams["Anthropic"]
							if exists {
								s, ok := raw.(WebSearchResultContentParam)
								if ok {
									results = append(results, s)
								}
							} else {
								results = append(results, WebSearchResultContentParam{
									Type:             "web_search_result",
									Url:              source.URL,
									Title:            source.URL,
									EncryptedContent: "",
									PageAge:          "",
								})
							}
						}

						// web_search_tool_result
						contents = append(contents, ContentUnion{
							OfWebSearchResult: &WebSearchResultContent{
								ToolUseId: nativeMessage.OfWebSearchCall.ID,
								Content:   results,
							},
						})

						out = append(out, MessageUnion{
							Role:    RoleAssistant,
							Content: ContentUnionParam{OfList: contents},
						})
					}
				}
			}

			if nativeMessage.OfCodeInterpreterCall != nil {
				var contents Contents

				// server_tool_use
				contents = append(contents, ContentUnion{
					OfServerToolUse: &ServerToolUseContent{
						Id:   nativeMessage.OfCodeInterpreterCall.ID,
						Name: "bash_code_execution",
						Input: struct {
							Query   string `json:"query"`
							Command string `json:"command"`
						}{Command: nativeMessage.OfCodeInterpreterCall.Code},
					},
				})

				// bash_code_execution_tool_result
				var stdout []string
				for _, o := range nativeMessage.OfCodeInterpreterCall.Outputs {
					stdout = append(stdout, o.Logs)
				}

				contents = append(contents, ContentUnion{
					OfBashCodeExecutionToolResult: &BashCodeExecutionResultContent{
						ToolUseId: nativeMessage.OfCodeInterpreterCall.ID,
						Content: BashCodeExecutionResultParam{
							Type:       "bash_code_execution_result",
							Stdout:     strings.Join(stdout, "\n"),
							Stderr:     "",
							ReturnCode: 0,
							Content:    nil,
						},
					},
				})

				out = append(out, MessageUnion{
					Role:    RoleAssistant,
					Content: ContentUnionParam{OfList: contents},
				})
			}
		}
	}

	return out
}

// anthropicUsage is the inverse of nativeUsage: it splits the native
// cache-inclusive InputTokens back into the shape Anthropic's wire format
// expects, where input_tokens is only the uncached remainder and the cached
// portion is reported beside it. Emitting InputTokens as-is would double-count
// the cached tokens for any client that sums the three fields, as Anthropic's
// own accounting does.
//
// Cache *writes* cannot be recovered on the way back — the native contract
// folds them into InputTokens without a separate detail field — so they are
// emitted as ordinary uncached prompt tokens. That keeps the prompt total
// exact and makes the round-trip lossless for InputTokens and TotalTokens,
// at the cost of the read/write split.
func anthropicUsage(in *responses2.Usage) *ChunkMessageUsage {
	if in == nil {
		return nil
	}

	cacheRead := in.InputTokensDetails.CachedTokens
	uncached := in.InputTokens - cacheRead
	if uncached < 0 {
		// CachedTokens is meant to be a subset of InputTokens; if a provider
		// ever violates that, keep the reported total rather than emitting a
		// negative count.
		uncached = 0
		cacheRead = in.InputTokens
	}

	return &ChunkMessageUsage{
		InputTokens:          uncached,
		CacheReadInputTokens: cacheRead,
		OutputTokens:         in.OutputTokens,
	}
}

func NativeResponseToResponse(in *responses2.Response) *Response {
	contents := Contents{}

	for _, nativeOutput := range in.Output {
		if nativeOutput.OfOutputMessage != nil {
			for _, nativeContent := range *nativeOutput.OfOutputMessage.Content {
				contents = append(contents, ContentUnion{
					OfText: &TextContent{
						Type:      "text",
						Text:      nativeContent.OfOutputText.Text,
						Citations: NativeAnnotationsToCitations(nativeContent.OfOutputText.Annotations),
					},
				})
			}
		}

		if nativeOutput.OfFunctionCall != nil {
			contents = append(contents, ContentUnion{
				OfToolUse: &ToolUseContent{
					ID:    nativeOutput.OfFunctionCall.ID,
					Name:  nativeOutput.OfFunctionCall.Name,
					Input: nativeOutput.OfFunctionCall.Arguments,
				},
			})
		}

		if nativeOutput.OfReasoning != nil {
			summaryText := ""
			for _, nativeSummaryContent := range nativeOutput.OfReasoning.Summary {
				summaryText += nativeSummaryContent.Text
			}

			if summaryText != "" {
				contents = append(contents, ContentUnion{
					OfThinking: &ThinkingContent{
						Thinking:  summaryText,
						Signature: *nativeOutput.OfReasoning.EncryptedContent,
					},
				})
			} else {
				contents = append(contents, ContentUnion{
					OfThinking: &ThinkingContent{
						Signature: *nativeOutput.OfReasoning.EncryptedContent,
					},
				})
			}
		}

		if nativeOutput.OfWebSearchCall != nil {
			if nativeOutput.OfWebSearchCall.Action.OfSearch != nil {
				// If the native message contains "sources" (obtained by include=["web_search_call.action.sources"]), then we will
				// map it to server_tool_use and web_search_tool_result
				// otherwise we will skip it
				if nativeOutput.OfWebSearchCall.Action.OfSearch.Sources != nil && len(nativeOutput.OfWebSearchCall.Action.OfSearch.Sources) > 0 {
					query := nativeOutput.OfWebSearchCall.Action.OfSearch.Query

					// server_tool_use
					contents = append(contents, ContentUnion{
						OfServerToolUse: &ServerToolUseContent{
							Id:   nativeOutput.OfWebSearchCall.ID,
							Name: "web_search",
							Input: struct {
								Query   string `json:"query"`
								Command string `json:"command"`
							}{
								Query: query,
							},
						},
					})

					results := []WebSearchResultContentParam{}
					for _, source := range nativeOutput.OfWebSearchCall.Action.OfSearch.Sources {
						raw, exists := source.ExtraParams["Anthropic"]
						if exists {
							s, ok := raw.(WebSearchResultContentParam)
							if ok {
								results = append(results, s)
							}
						} else {
							results = append(results, WebSearchResultContentParam{
								Type:             "web_search_result",
								Url:              source.URL,
								Title:            source.URL,
								EncryptedContent: "",
								PageAge:          "",
							})
						}
					}

					// web_search_tool_result
					contents = append(contents, ContentUnion{
						OfWebSearchResult: &WebSearchResultContent{
							ToolUseId: nativeOutput.OfWebSearchCall.ID,
							Content:   results,
						},
					})
				}
			}
		}

		if nativeOutput.OfCodeInterpreterCall != nil {
			// server_tool_use
			contents = append(contents, ContentUnion{
				OfServerToolUse: &ServerToolUseContent{
					Id:   nativeOutput.OfCodeInterpreterCall.ID,
					Name: "bash_code_execution",
					Input: struct {
						Query   string `json:"query"`
						Command string `json:"command"`
					}{Command: nativeOutput.OfCodeInterpreterCall.Code},
				},
			})

			// bash_code_execution_tool_result
			var stdout []string
			for _, o := range nativeOutput.OfCodeInterpreterCall.Outputs {
				stdout = append(stdout, o.Logs)
			}

			contents = append(contents, ContentUnion{
				OfBashCodeExecutionToolResult: &BashCodeExecutionResultContent{
					ToolUseId: nativeOutput.OfCodeInterpreterCall.ID,
					Content: BashCodeExecutionResultParam{
						Type:       "bash_code_execution_result",
						Stdout:     strings.Join(stdout, "\n"),
						Stderr:     "",
						ReturnCode: 0,
						Content:    nil,
					},
				},
			})
		}
	}

	var stopReason StopReason
	var stopSequence string
	if in.Metadata != nil {
		if val, ok := in.Metadata["stop_reason"]; ok {
			stopReason = val.(StopReason)
		}
		if val, ok := in.Metadata["stop_sequence"]; ok {
			stopSequence = val.(string)
		}
	}

	return &Response{
		Model:        in.Model,
		Id:           in.ID,
		Type:         "message",
		Role:         RoleAssistant,
		Content:      contents,
		StopReason:   stopReason,
		StopSequence: stopSequence,
		Usage:        anthropicUsage(in.Usage),
		ServiceTier:  in.ServiceTier,
		Error:        nil,
	}
}

// =============================================================================
// Native to ResponseChunk Conversion
// =============================================================================

// NativeResponseChunkToResponseChunkConverter converts native stream chunks to Anthropic format.
// This converter is stateless since native format contains all necessary information in each chunk.
type NativeResponseChunkToResponseChunkConverter struct {
	OfResponseCreated *responses2.ChunkResponse[constants.ChunkTypeResponseCreated]
	outputIndex       int
}

// NativeResponseChunkToResponseChunk converts a single native chunk to zero or more Anthropic chunks.
func (c *NativeResponseChunkToResponseChunkConverter) NativeResponseChunkToResponseChunk(in *responses2.ResponseChunk) []ResponseChunk {
	if in == nil {
		return nil
	}

	switch {
	case in.OfResponseCreated != nil:
		return c.handleResponseCreated(in.OfResponseCreated)
	case in.OfResponseInProgress != nil:
		return nil // No Anthropic equivalent
	case in.OfOutputItemAdded != nil:
		return c.handleOutputItemAdded(in.OfOutputItemAdded)
	case in.OfContentPartAdded != nil:
		return c.handleContentPartAdded(in.OfContentPartAdded)
	case in.OfOutputTextDelta != nil:
		return c.handleOutputTextDelta(in.OfOutputTextDelta)
	case in.OfOutputTextAnnotationAdded != nil:
		return c.handleOutputTextAnnotationAdded(in.OfOutputTextAnnotationAdded)
	case in.OfOutputTextDone != nil:
		return nil // No Anthropic equivalent (block stop handles this)
	case in.OfContentPartDone != nil:
		return nil // No Anthropic equivalent
	case in.OfFunctionCallArgumentsDelta != nil:
		return c.handleFunctionCallArgumentsDelta(in.OfFunctionCallArgumentsDelta)
	case in.OfFunctionCallArgumentsDone != nil:
		return nil // No Anthropic equivalent
	case in.OfReasoningSummaryPartAdded != nil:
		return c.handleReasoningSummaryPartAdded(in.OfReasoningSummaryPartAdded)
	case in.OfReasoningSummaryTextDelta != nil:
		return c.handleReasoningSummaryTextDelta(in.OfReasoningSummaryTextDelta)
	case in.OfReasoningSummaryTextDone != nil:
		return nil // No Anthropic equivalent
	case in.OfReasoningSummaryPartDone != nil:
		return nil // No Anthropic equivalent
	case in.OfWebSearchCallInProgress != nil:
		return nil // No Anthropic equivalent
	case in.OfWebSearchCallSearching != nil:
		return nil // No Anthropic equivalent
	case in.OfWebSearchCallCompleted != nil:
		return nil // No Anthropic equivalent
	case in.OfCodeInterpreterCallInProgress != nil:
		return nil // No Anthropic equivalent
	case in.OfCodeInterpreterCallCodeDelta != nil:
		return c.handleCodeInterpreterCallCodeDelta(in.OfCodeInterpreterCallCodeDelta)
	case in.OfCodeInterpreterCallCodeDone != nil:
		return c.handleCodeInterpreterCallCodeDone(in.OfCodeInterpreterCallCodeDone)
	case in.OfCodeInterpreterCallInterpreting != nil:
		return nil // No Anthropic equivalent
	case in.OfCodeInterpreterCallCompleted != nil:
		return nil // No Anthropic equivalent
	case in.OfOutputItemDone != nil:
		return c.handleOutputItemDone(in.OfOutputItemDone)
	case in.OfResponseCompleted != nil:
		return c.handleResponseCompleted(in.OfResponseCompleted)
	}

	return nil
}

// =============================================================================
// Event Handlers
// =============================================================================

// handleResponseCreated emits message_start
func (c *NativeResponseChunkToResponseChunkConverter) handleResponseCreated(resp *responses2.ChunkResponse[constants.ChunkTypeResponseCreated]) []ResponseChunk {
	c.OfResponseCreated = resp
	return []ResponseChunk{
		c.buildMessageStart(resp.Response.Id, resp.Response.Request.Model),
	}
}

// handleOutputItemAdded emits content_block_start for function_call only
// (text/message items defer to content_part.added since content type isn't known yet)
func (c *NativeResponseChunkToResponseChunkConverter) handleOutputItemAdded(item *responses2.ChunkOutputItem[constants.ChunkTypeOutputItemAdded]) []ResponseChunk {
	if item.Item.Type == "function_call" {
		return []ResponseChunk{
			c.buildContentBlockStartToolUse(item.OutputIndex, *item.Item.CallID, *item.Item.Name, item.Item.Arguments),
		}
	}

	if item.Item.Type == "web_search_call" {
		return []ResponseChunk{
			c.buildContentBlockStartServerToolUse("web_search", item.OutputIndex, item.Item.Id),
		}
	}

	if item.Item.Type == "code_interpreter_call" {
		return []ResponseChunk{
			c.buildContentBlockStartServerToolUse("bash_code_execution", item.OutputIndex, item.Item.Id),
		}
	}

	return nil
}

// handleContentPartAdded emits content_block_start for text
func (c *NativeResponseChunkToResponseChunkConverter) handleContentPartAdded(part *responses2.ChunkContentPart[constants.ChunkTypeContentPartAdded]) []ResponseChunk {
	if part.Part.OfOutputText == nil {
		return nil
	}
	return []ResponseChunk{
		c.buildContentBlockStartText(part.OutputIndex, part.Part.OfOutputText.Text),
	}
}

// handleOutputTextDelta emits content_block_delta with text_delta
func (c *NativeResponseChunkToResponseChunkConverter) handleOutputTextDelta(delta *responses2.ChunkOutputText[constants.ChunkTypeOutputTextDelta]) []ResponseChunk {
	return []ResponseChunk{
		c.buildContentBlockDeltaText(delta.OutputIndex, delta.Delta),
	}
}

func (c *NativeResponseChunkToResponseChunkConverter) handleOutputTextAnnotationAdded(delta *responses2.ChunkOutputText[constants.ChunkTypeOutputTextAnnotationAdded]) []ResponseChunk {
	return []ResponseChunk{
		c.buildContentBlockDeltaCitation(delta.OutputIndex, *delta.Annotation),
	}
}

// handleFunctionCallArgumentsDelta emits content_block_delta with input_json_delta
func (c *NativeResponseChunkToResponseChunkConverter) handleFunctionCallArgumentsDelta(delta *responses2.ChunkFunctionCall[constants.ChunkTypeFunctionCallArgumentsDelta]) []ResponseChunk {
	return []ResponseChunk{
		c.buildContentBlockDeltaInputJSON(delta.OutputIndex, delta.Arguments),
	}
}

// handleReasoningSummaryPartAdded emits content_block_start for thinking
func (c *NativeResponseChunkToResponseChunkConverter) handleReasoningSummaryPartAdded(part *responses2.ChunkReasoningSummaryPart[constants.ChunkTypeReasoningSummaryPartAdded]) []ResponseChunk {
	return []ResponseChunk{
		c.buildContentBlockStartThinking(part.OutputIndex),
	}
}

// handleReasoningSummaryTextDelta emits content_block_delta with thinking or signature
func (c *NativeResponseChunkToResponseChunkConverter) handleReasoningSummaryTextDelta(delta *responses2.ChunkReasoningSummaryText[constants.ChunkTypeReasoningSummaryTextDelta]) []ResponseChunk {
	if delta.EncryptedContent != nil {
		return []ResponseChunk{c.buildContentBlockDeltaSignature(delta.SummaryIndex, *delta.EncryptedContent)}
	}
	return []ResponseChunk{c.buildContentBlockDeltaThinking(delta.SummaryIndex, delta.Delta)}
}

func (c *NativeResponseChunkToResponseChunkConverter) handleCodeInterpreterCallCodeDelta(delta *responses2.ChunkCodeInterpreterCall[constants.ChunkTypeCodeInterpreterCallCodeDelta]) []ResponseChunk {
	return []ResponseChunk{
		c.buildContentBlockDeltaInputJSON(delta.OutputIndex, *delta.Delta),
	}
}

func (c *NativeResponseChunkToResponseChunkConverter) handleCodeInterpreterCallCodeDone(code *responses2.ChunkCodeInterpreterCall[constants.ChunkTypeCodeInterpreterCallCodeDone]) []ResponseChunk {
	return []ResponseChunk{
		c.buildContentBlockStop(code.OutputIndex),
	}
}

func (c *NativeResponseChunkToResponseChunkConverter) handleCodeInterpreterCallCompleted(codeInterpreterCall *responses2.ChunkCodeInterpreterCall[constants.ChunkTypeCodeInterpreterCallCompleted]) []ResponseChunk {
	return []ResponseChunk{}
}

// handleOutputItemDone emits content_block_stop
func (c *NativeResponseChunkToResponseChunkConverter) handleOutputItemDone(item *responses2.ChunkOutputItem[constants.ChunkTypeOutputItemDone]) []ResponseChunk {
	if item.Item.Type == "web_search_call" {
		chunks := []ResponseChunk{}
		if item.Item.Action.OfSearch != nil {
			inputQuery := struct {
				Query string `json:"query"`
			}{
				Query: item.Item.Action.OfSearch.Query,
			}
			buf, err := sonic.Marshal(inputQuery)
			if err == nil {
				chunks = append(chunks, c.buildContentBlockDeltaInputJSON(item.OutputIndex, string(buf)))
			}
		}

		chunks = append(chunks, c.buildContentBlockStop(item.OutputIndex))
		item.OutputIndex++
		var resultContents []WebSearchResultContentParam
		for _, source := range item.Item.Action.OfSearch.Sources {
			if raw, exists := source.ExtraParams["Anthropic"]; exists {
				if anthropicResult, ok := raw.(WebSearchResultContentParam); ok {
					resultContents = append(resultContents, anthropicResult)
				}
			} else {
				resultContents = append(resultContents, WebSearchResultContentParam{
					Type:             "web_search_result",
					Url:              source.URL,
					Title:            source.URL,
					EncryptedContent: "",
					PageAge:          "",
				})
			}
		}
		chunks = append(chunks, ResponseChunk{
			OfContentBlockStart: &ChunkContentBlock[ChunkTypeContentBlockStart]{
				Index: c.outputIndex,
				ContentBlock: &ContentUnion{
					OfWebSearchResult: &WebSearchResultContent{
						ToolUseId: item.Item.Id,
						Content:   resultContents,
					},
				},
			},
		})
		chunks = append(chunks, c.buildContentBlockStop(item.OutputIndex))
		return chunks
	}

	if item.Item.Type == "code_interpreter_call" {
		chunks := []ResponseChunk{}

		chunks = append(chunks, ResponseChunk{
			OfContentBlockStart: &ChunkContentBlock[ChunkTypeContentBlockStart]{
				Index: c.outputIndex,
				ContentBlock: &ContentUnion{
					OfBashCodeExecutionToolResult: &BashCodeExecutionResultContent{
						ToolUseId: item.Item.Id,
						Content: BashCodeExecutionResultParam{
							Type:       "bash_code_execution_result",
							Stdout:     item.Item.Outputs[0].Logs,
							Stderr:     "",
							ReturnCode: 0,
							Content:    nil,
						},
					},
				},
			},
		})
		chunks = append(chunks, c.buildContentBlockStop(item.OutputIndex))

		return chunks
	}

	return []ResponseChunk{
		c.buildContentBlockStop(item.OutputIndex),
	}
}

// handleResponseCompleted emits message_delta and message_stop
func (c *NativeResponseChunkToResponseChunkConverter) handleResponseCompleted(resp *responses2.ChunkResponse[constants.ChunkTypeResponseCompleted]) []ResponseChunk {
	stopReason := "end_turn"
	if resp.Response.Status == "incomplete" {
		stopReason = "max_tokens"
	}

	return []ResponseChunk{
		c.buildMessageDelta(stopReason, resp.Response.Usage),
		c.buildMessageStop(),
	}
}

// =============================================================================
// Chunk Builders
// =============================================================================

func (c *NativeResponseChunkToResponseChunkConverter) buildMessageStart(id, model string) ResponseChunk {
	return ResponseChunk{
		OfMessageStart: &ChunkMessage[ChunkTypeMessageStart]{
			Type: ChunkTypeMessageStart("message_start"),
			Message: &ChunkMessageData{
				Id:      id,
				Type:    "message",
				Role:    "assistant",
				Model:   model,
				Content: []interface{}{},
				Usage:   &ChunkMessageUsage{InputTokens: 0, OutputTokens: 0},
			},
		},
	}
}

func (c *NativeResponseChunkToResponseChunkConverter) buildContentBlockStartText(index int, text string) ResponseChunk {
	return ResponseChunk{
		OfContentBlockStart: &ChunkContentBlock[ChunkTypeContentBlockStart]{
			Type:         ChunkTypeContentBlockStart("content_block_start"),
			Index:        c.outputIndex,
			ContentBlock: &ContentUnion{OfText: &TextContent{Text: text}},
		},
	}
}

func (c *NativeResponseChunkToResponseChunkConverter) buildContentBlockStartToolUse(index int, callID, name string, args any) ResponseChunk {
	return ResponseChunk{
		OfContentBlockStart: &ChunkContentBlock[ChunkTypeContentBlockStart]{
			Type:  ChunkTypeContentBlockStart("content_block_start"),
			Index: c.outputIndex,
			ContentBlock: &ContentUnion{
				OfToolUse: &ToolUseContent{ID: callID, Name: name, Input: args},
			},
		},
	}
}

func (c *NativeResponseChunkToResponseChunkConverter) buildContentBlockStartThinking(index int) ResponseChunk {
	return ResponseChunk{
		OfContentBlockStart: &ChunkContentBlock[ChunkTypeContentBlockStart]{
			Type:  ChunkTypeContentBlockStart("content_block_start"),
			Index: c.outputIndex,
			ContentBlock: &ContentUnion{
				OfThinking: &ThinkingContent{Thinking: "", Signature: ""},
			},
		},
	}
}

func (c *NativeResponseChunkToResponseChunkConverter) buildContentBlockStartServerToolUse(name string, index int, id string) ResponseChunk {
	return ResponseChunk{
		OfContentBlockStart: &ChunkContentBlock[ChunkTypeContentBlockStart]{
			Type:  ChunkTypeContentBlockStart("content_block_start"),
			Index: c.outputIndex,
			ContentBlock: &ContentUnion{
				OfServerToolUse: &ServerToolUseContent{
					Id:   id,
					Name: name,
					Input: struct {
						Query   string `json:"query"`
						Command string `json:"command"`
					}{Query: ""},
				},
			},
		},
	}
}

func (c *NativeResponseChunkToResponseChunkConverter) buildContentBlockStartBashCodeExecutionToolResult(index int, id string, output string) ResponseChunk {
	return ResponseChunk{
		OfContentBlockStart: &ChunkContentBlock[ChunkTypeContentBlockStart]{
			Type:  ChunkTypeContentBlockStart("content_block_start"),
			Index: c.outputIndex,
			ContentBlock: &ContentUnion{
				OfBashCodeExecutionToolResult: &BashCodeExecutionResultContent{
					Type:      ContentTypeBashCodeExecutionToolResultContent(""),
					ToolUseId: id,
					Content: BashCodeExecutionResultParam{
						Type:       "bash_code_execution_result",
						Stdout:     output,
						Stderr:     "",
						ReturnCode: 0,
						Content:    nil,
					},
				},
			},
		},
	}
}

func (c *NativeResponseChunkToResponseChunkConverter) buildContentBlockDeltaText(index int, text string) ResponseChunk {
	return ResponseChunk{
		OfContentBlockDelta: &ChunkContentBlock[ChunkTypeContentBlockDelta]{
			Type:  ChunkTypeContentBlockDelta("content_block_delta"),
			Index: c.outputIndex,
			Delta: &ChunkContentBlockDeltaUnion{
				OfText: &DeltaTextContent{Type: "text_delta", Text: text},
			},
		},
	}
}

func (c *NativeResponseChunkToResponseChunkConverter) buildContentBlockDeltaCitation(index int, annotation responses2.Annotation) ResponseChunk {
	citations := NativeAnnotationsToCitations([]responses2.Annotation{annotation})

	return ResponseChunk{
		OfContentBlockDelta: &ChunkContentBlock[ChunkTypeContentBlockDelta]{
			Type:  ChunkTypeContentBlockDelta("content_block_delta"),
			Index: c.outputIndex,
			Delta: &ChunkContentBlockDeltaUnion{
				OfCitation: &DeltaCitation{
					Citation: citations[0],
				},
			},
		},
	}
}

func (c *NativeResponseChunkToResponseChunkConverter) buildContentBlockDeltaInputJSON(index int, json string) ResponseChunk {
	return ResponseChunk{
		OfContentBlockDelta: &ChunkContentBlock[ChunkTypeContentBlockDelta]{
			Type:  ChunkTypeContentBlockDelta("content_block_delta"),
			Index: c.outputIndex,
			Delta: &ChunkContentBlockDeltaUnion{
				OfInputJSON: &DeltaInputJSONContent{Type: "input_json_delta", PartialJSON: json},
			},
		},
	}
}

func (c *NativeResponseChunkToResponseChunkConverter) buildContentBlockDeltaThinking(index int, thinking string) ResponseChunk {
	return ResponseChunk{
		OfContentBlockDelta: &ChunkContentBlock[ChunkTypeContentBlockDelta]{
			Type:  ChunkTypeContentBlockDelta("content_block_delta"),
			Index: c.outputIndex,
			Delta: &ChunkContentBlockDeltaUnion{
				OfThinking: &DeltaThinkingContent{Thinking: thinking},
			},
		},
	}
}

func (c *NativeResponseChunkToResponseChunkConverter) buildContentBlockDeltaSignature(index int, sig string) ResponseChunk {
	return ResponseChunk{
		OfContentBlockDelta: &ChunkContentBlock[ChunkTypeContentBlockDelta]{
			Type:  ChunkTypeContentBlockDelta("content_block_delta"),
			Index: c.outputIndex,
			Delta: &ChunkContentBlockDeltaUnion{
				OfThinkingSignature: &DeltaThinkingSignatureContent{Signature: sig},
			},
		},
	}
}

func (c *NativeResponseChunkToResponseChunkConverter) buildContentBlockStop(index int) ResponseChunk {
	out := ResponseChunk{
		OfContentBlockStop: &ChunkContentBlock[ChunkTypeContentBlockStop]{
			Type:  ChunkTypeContentBlockStop("content_block_stop"),
			Index: c.outputIndex,
		},
	}

	c.outputIndex++

	return out
}

func (c *NativeResponseChunkToResponseChunkConverter) buildMessageDelta(stopReason string, usage responses2.Usage) ResponseChunk {
	return ResponseChunk{
		OfMessageDelta: &ChunkMessage[ChunkTypeMessageDelta]{
			Type: ChunkTypeMessageDelta("message_delta"),
			Message: &ChunkMessageData{
				StopReason:   stopReason,
				StopSequence: nil,
			},
			Usage: anthropicUsage(&usage),
		},
	}
}

func (c *NativeResponseChunkToResponseChunkConverter) buildMessageStop() ResponseChunk {
	return ResponseChunk{
		OfMessageStop: &ChunkMessage[ChunkTypeMessageStop]{
			Type: ChunkTypeMessageStop("message_stop"),
		},
	}
}

func IsAdaptiveThinkingModel(model string) bool {
	return strings.Contains(model, "4-6") || strings.Contains(model, "4-7")
}
