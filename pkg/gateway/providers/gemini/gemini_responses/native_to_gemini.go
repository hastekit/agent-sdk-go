package gemini_responses

import (
	"log/slog"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	responses2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

func ResponsesInputToGeminiResponsesInput(in *responses2.Request) *Request {
	if in.MaxToolCalls != nil {
		slog.Warn("max tool call is not supported for anthropic models")
	}

	if in.ParallelToolCalls != nil {
		slog.Warn("parallel tool call is not supported for anthropic models")
	}

	out := &Request{
		Model:    in.Model,
		Contents: NativeMessagesToMessages(in.Input),
		GenerationConfig: &GenerationConfig{
			Temperature:     in.Temperature,
			MaxOutputTokens: in.MaxOutputTokens,
			TopP:            in.TopP,
			TopK:            in.TopLogprobs,
		},
		Tools:  NativeToolsToTools(in.Tools),
		Stream: in.Stream,
	}

	out.GenerationConfig.ThinkingConfig = NativeReasoningParamToGeminiThinkingConfig(in)

	if in.Instructions != nil {
		out.SystemInstruction = &Content{
			Parts: []Part{
				{
					Text: in.Instructions,
				},
			},
			Role: RoleSystem,
		}
	}

	// Gemini doesn't allow structured outputs while also having tools
	if in.Text != nil && in.Text.Format != nil {
		if in.Tools == nil || len(in.Tools) == 0 {
			if schema, ok := in.Text.Format["schema"].(map[string]any); ok {
				out.GenerationConfig.ResponseMimeType = utils.Ptr("application/json")
				out.GenerationConfig.ResponseJsonSchema = schema
			}
		} else {
			slog.Warn("structured output is not supported while tools are provided")
		}
	}

	return out
}

func NativeReasoningParamToGeminiThinkingConfig(in *responses2.Request) *ThinkingConfig {
	if in.Reasoning == nil {
		return nil
	}

	var effort string // "LOW" or "HIGH"
	switch *in.Reasoning.Effort {
	case "none", "minimal", "low", "medium":
		effort = "LOW"
	case "high", "xhigh":
		effort = "HIGH"
	}

	return &ThinkingConfig{
		IncludeThoughts: utils.Ptr(true),
		ThinkingBudget:  in.Reasoning.BudgetTokens,
		ThinkingLevel:   utils.Ptr(effort),
	}
}

func NativeRoleToRole(role constants.Role) Role {
	switch role {
	case constants.RoleUser:
		return RoleUser

	case constants.RoleSystem, constants.RoleDeveloper:
		return RoleUser

	case constants.RoleAssistant:
		return RoleModel
	}

	return RoleUser
}

func NativeToolsToTools(nativeTools []responses2.ToolUnion) []Tool {
	out := Tool{
		FunctionDeclarations: []FunctionTool{},
	}

	for _, nativeTool := range nativeTools {
		if nativeTool.OfFunction != nil {
			out.FunctionDeclarations = append(out.FunctionDeclarations, FunctionTool{
				Name:                 nativeTool.OfFunction.Name,
				Description:          *nativeTool.OfFunction.Description,
				ParametersJsonSchema: nativeTool.OfFunction.Parameters,
				ResponseJsonSchema:   nil,
			})
		}

		if nativeTool.OfCodeExecution != nil {
			out.CodeExecution = &CodeExecutionTool{}
		}
	}

	return []Tool{out}
}

func NativeMessagesToMessages(in responses2.InputUnion) []Content {
	out := []Content{}

	if in.OfString != nil {
		out = append(out, Content{
			Role: RoleUser,
			Parts: []Part{
				{Text: in.OfString},
			},
		})

		return out
	}

	if in.OfInputMessageList != nil {
		prevFunctionCallName := ""
		for _, nativeMessage := range in.OfInputMessageList {
			// Easy Message
			if nativeMessage.OfEasyInput != nil {
				parts := []Part{}

				if nativeMessage.OfEasyInput.Content.OfString != nil {
					parts = append(parts, Part{
						Text: nativeMessage.OfEasyInput.Content.OfString,
					})
				}

				if nativeMessage.OfEasyInput.Content.OfInputMessageList != nil {
					for _, nativeContent := range nativeMessage.OfEasyInput.Content.OfInputMessageList {
						if nativeContent.OfInputText != nil {
							parts = append(parts, Part{
								Text: utils.Ptr(nativeContent.OfInputText.Text),
							})
						}

						if nativeContent.OfOutputText != nil {
							parts = append(parts, Part{
								Text: utils.Ptr(nativeContent.OfOutputText.Text),
							})
						}

						if nativeContent.OfInputImage != nil {
							// Data URLs
							if strings.HasPrefix(*nativeContent.OfInputImage.ImageURL, "data:") {
								contentType, data, err := utils.ParseDataURL(*nativeContent.OfInputImage.ImageURL)
								if err != nil {
									slog.Warn("error in parsing data url")
									continue
								}

								parts = append(parts, Part{
									InlineData: &InlinePartData{
										MimeType: contentType,
										Data:     data,
									},
								})
							}

							// TODO: URLs
						}
					}
				}

				out = append(out, Content{
					Role:  NativeRoleToRole(nativeMessage.OfEasyInput.Role),
					Parts: parts,
				})
			}

			// Text Message
			if nativeMessage.OfInputMessage != nil {
				parts := []Part{}

				for _, nativeContent := range nativeMessage.OfInputMessage.Content {
					if nativeContent.OfInputText != nil {
						parts = append(parts, Part{
							Text: utils.Ptr(nativeContent.OfInputText.Text),
						})
					}

					if nativeContent.OfOutputText != nil {
						parts = append(parts, Part{
							Text: utils.Ptr(nativeContent.OfOutputText.Text),
						})
					}

					if nativeContent.OfInputImage != nil {
						// Data URLs
						if strings.HasPrefix(*nativeContent.OfInputImage.ImageURL, "data:") {
							contentType, data, err := utils.ParseDataURL(*nativeContent.OfInputImage.ImageURL)
							if err != nil {
								slog.Warn("error in parsing data url")
								continue
							}

							parts = append(parts, Part{
								InlineData: &InlinePartData{
									MimeType: contentType,
									Data:     data,
								},
							})
						}

						// TODO: URLs
					}
				}

				out = append(out, Content{
					Role:  NativeRoleToRole(nativeMessage.OfInputMessage.Role),
					Parts: parts,
				})
			}

			// Function call
			if nativeMessage.OfFunctionCall != nil {
				args := map[string]any{}
				err := sonic.Unmarshal([]byte(nativeMessage.OfFunctionCall.Arguments), &args)
				if err != nil {
					slog.Warn("error in unmarshalling function arg into map[string]any")
				}

				out = append(out, Content{
					Role: RoleModel,
					Parts: []Part{
						{
							FunctionCall: &FunctionCall{
								Name: nativeMessage.OfFunctionCall.Name,
								Args: args,
							},
							ThoughtSignature: nativeMessage.OfFunctionCall.ThoughtSignature,
						},
					},
				})
				prevFunctionCallName = nativeMessage.OfFunctionCall.Name
			}

			// Function call output
			if nativeMessage.OfFunctionCallOutput != nil {
				parts := []Part{}

				if nativeMessage.OfFunctionCallOutput.Output.OfString != nil {
					parts = append(parts, Part{
						FunctionResponse: &FunctionResponse{
							ID:   nativeMessage.OfFunctionCallOutput.CallID,
							Name: prevFunctionCallName,
							Response: map[string]any{
								"output": nativeMessage.OfFunctionCallOutput.Output.OfString,
							},
						},
					})
				}

				if nativeMessage.OfFunctionCallOutput.Output.OfList != nil {
					for _, nativeOutput := range nativeMessage.OfFunctionCallOutput.Output.OfList {
						if nativeOutput.OfInputText != nil {
							parts = append(parts, Part{
								FunctionResponse: &FunctionResponse{
									ID:   nativeMessage.OfFunctionCallOutput.CallID,
									Name: prevFunctionCallName,
									Response: map[string]any{
										"output": nativeOutput.OfInputText.Text,
									},
								},
							})
						}
					}
				}

				out = append(out, Content{
					Role:  RoleUser,
					Parts: parts,
				})
			}

			// Reasoning
			if nativeMessage.OfReasoning != nil {

			}

			// Image Generation Call
			if nativeMessage.OfImageGenerationCall != nil {
				out = append(out, Content{
					Parts: []Part{
						{
							InlineData: &InlinePartData{
								MimeType: "image/" + nativeMessage.OfImageGenerationCall.OutputFormat,
								Data:     nativeMessage.OfImageGenerationCall.Result,
							},
						},
					},
				})
			}

			// Code Interpreter Call
			if nativeMessage.OfCodeInterpreterCall != nil {
				outputs := []string{}
				for _, o := range nativeMessage.OfCodeInterpreterCall.Outputs {
					outputs = append(outputs, o.Logs)
				}

				out = append(out, Content{
					Parts: []Part{
						{
							ExecutableCode: &ExecutableCodePart{
								Language: "",
								Code:     nativeMessage.OfCodeInterpreterCall.Code,
							},
						},
						{
							CodeExecutionResult: &CodeExecutionResultPart{
								Outcome: "OUTCOME_OK",
								Output:  strings.Join(outputs, "\n"),
							},
						},
					},
				})
			}
		}
	}

	return out
}

func NativeResponseToResponse(in *responses2.Response) *Response {
	parts := []Part{}

	for _, nativeOutput := range in.Output {
		if nativeOutput.OfOutputMessage != nil {
			for _, nativeContent := range *nativeOutput.OfOutputMessage.Content {
				parts = append(parts, Part{
					Text: utils.Ptr(nativeContent.OfOutputText.Text),
				})
			}
		}

		if nativeOutput.OfFunctionCall != nil {
			parts = append(parts, Part{
				FunctionCall: &FunctionCall{
					Name: nativeOutput.OfFunctionCall.Name,
					Args: nativeOutput.OfFunctionCall.Arguments,
				},
			})
		}

		if nativeOutput.OfCodeInterpreterCall != nil {
			outputs := []string{}
			for _, o := range nativeOutput.OfCodeInterpreterCall.Outputs {
				outputs = append(outputs, o.Logs)
			}

			parts = append(parts, Part{
				ExecutableCode: &ExecutableCodePart{
					Language: "",
					Code:     nativeOutput.OfCodeInterpreterCall.Code,
				},
			})

			parts = append(parts, Part{
				CodeExecutionResult: &CodeExecutionResultPart{
					Outcome: "OUTCOME_OK",
					Output:  strings.Join(outputs, "\n"),
				},
			})
		}
	}

	var stopReason string
	if in.Metadata != nil {
		if val, ok := in.Metadata["stop_reason"]; ok {
			stopReason = val.(string)
		}
	}

	return &Response{
		ModelVersion: in.Model,
		ResponseID:   in.ID,
		UsageMetadata: &UsageMetadata{
			PromptTokenCount:     in.Usage.InputTokens,
			CandidatesTokenCount: in.Usage.OutputTokens,
			TotalTokenCount:      in.Usage.TotalTokens,
			PromptTokensDetails:  nil,
			ThoughtsTokenCount:   in.Usage.OutputTokensDetails.ReasoningTokens,
		},
		Candidates: []Candidate{
			{
				Content: Content{
					Role:  RoleModel,
					Parts: parts,
				},
				FinishReason: stopReason,
			},
		},
		Error: nil,
	}
}

// =============================================================================
// Native to Gemini ResponseChunk Conversion
// =============================================================================

// NativeResponseChunkToResponseChunkConverter converts native stream chunks to Gemini format.
// Gemini expects Response objects with parts, so we only emit responses for content-bearing events.
type NativeResponseChunkToResponseChunkConverter struct {
	// Stored state from response.created for building Gemini responses
	responseCreated *responses2.ChunkResponse[constants.ChunkTypeResponseCreated]
}

// NativeResponseChunkToResponseChunk converts a native chunk to zero or more Gemini responses.
// Many native events don't map to Gemini output (Gemini doesn't have the same granular events).
func (c *NativeResponseChunkToResponseChunkConverter) NativeResponseChunkToResponseChunk(in *responses2.ResponseChunk) []Response {
	if in == nil {
		return nil
	}

	switch {
	case in.OfResponseCreated != nil:
		return c.handleResponseCreated(in.OfResponseCreated)
	case in.OfOutputTextDelta != nil:
		return c.handleOutputTextDelta(in.OfOutputTextDelta)
	case in.OfReasoningSummaryTextDelta != nil:
		return c.handleReasoningSummaryTextDelta(in.OfReasoningSummaryTextDelta)
	case in.OfOutputItemAdded != nil:
		return c.handleOutputItemAdded(in.OfOutputItemAdded)
	case in.OfCodeInterpreterCallCodeDone != nil:
		return c.handleCodeInterpreterCallCodeDone(in.OfCodeInterpreterCallCodeDone)
	case in.OfOutputItemDone != nil:
		return c.handleOutputItemDone(in.OfOutputItemDone)
	case in.OfResponseCompleted != nil:
		return c.handleResponseCompleted(in.OfResponseCompleted)
	}

	// Most native events don't map to Gemini output:
	// - response.in_progress, output_text.done, content_part.added/done,
	// - function_call_arguments.delta/done, response.completed
	// - reasoning events (Gemini handles thinking differently)
	return nil
}

// =============================================================================
// Event Handlers
// =============================================================================

// handleResponseCreated stores state but doesn't emit Gemini output
// (Gemini doesn't have a "response started" event - it just sends content)
func (c *NativeResponseChunkToResponseChunkConverter) handleResponseCreated(resp *responses2.ChunkResponse[constants.ChunkTypeResponseCreated]) []Response {
	c.responseCreated = resp
	return nil
}

// handleOutputTextDelta emits a Gemini response with a text part
func (c *NativeResponseChunkToResponseChunkConverter) handleOutputTextDelta(delta *responses2.ChunkOutputText[constants.ChunkTypeOutputTextDelta]) []Response {
	if c.responseCreated == nil {
		return nil
	}

	return []Response{
		c.buildResponse([]Part{{Text: utils.Ptr(delta.Delta)}}),
	}
}

// handleReasoningSummaryTextDelta emits Gemini response with a thought part
func (c *NativeResponseChunkToResponseChunkConverter) handleReasoningSummaryTextDelta(delta *responses2.ChunkReasoningSummaryText[constants.ChunkTypeReasoningSummaryTextDelta]) []Response {
	if c.responseCreated == nil {
		return nil
	}

	return []Response{
		c.buildResponse([]Part{{Text: utils.Ptr(delta.Delta), Thought: utils.Ptr(true)}}),
	}
}

func (c *NativeResponseChunkToResponseChunkConverter) handleCodeInterpreterCallCodeDone(code *responses2.ChunkCodeInterpreterCall[constants.ChunkTypeCodeInterpreterCallCodeDone]) []Response {
	if c.responseCreated == nil {
		return nil
	}

	return []Response{
		c.buildResponse([]Part{
			{
				ExecutableCode:   &ExecutableCodePart{Code: *code.Code},
				ThoughtSignature: code.ThoughtSignature,
			},
		}),
	}
}

// handleOutputItemAdded emits a Gemini response for function calls only
// (text items don't emit here - they use output_text.delta)
func (c *NativeResponseChunkToResponseChunkConverter) handleOutputItemAdded(item *responses2.ChunkOutputItem[constants.ChunkTypeOutputItemAdded]) []Response {
	if c.responseCreated == nil {
		return nil
	}

	if item.Item.Type == "function_call" {
		return []Response{
			c.buildResponse([]Part{{
				FunctionCall: &FunctionCall{
					Name: *item.Item.Name,
					Args: item.Item.Arguments,
				},
				ThoughtSignature: item.Item.ThoughtSignature,
			}}),
		}
	}

	return nil
}

func (c *NativeResponseChunkToResponseChunkConverter) handleOutputItemDone(item *responses2.ChunkOutputItem[constants.ChunkTypeOutputItemDone]) []Response {
	if c.responseCreated == nil {
		return nil
	}

	if item.Item.Type == "code_interpreter_call" {
		outputs := []string{}
		for _, o := range item.Item.Outputs {
			outputs = append(outputs, o.Logs)
		}

		return []Response{
			c.buildResponse([]Part{{CodeExecutionResult: &CodeExecutionResultPart{Outcome: "OUTCOME_OK", Output: strings.Join(outputs, "\n")}}}),
		}
	}

	return nil
}

func (c *NativeResponseChunkToResponseChunkConverter) handleResponseCompleted(item *responses2.ChunkResponse[constants.ChunkTypeResponseCompleted]) []Response {
	if c.responseCreated == nil {
		return nil
	}

	return []Response{
		{
			Candidates: []Candidate{{
				Content: Content{
					Role: RoleModel,
					Parts: []Part{
						{
							Text: utils.Ptr(""),
						},
					},
				},
				FinishReason: "STOP",
			}},
			UsageMetadata: &UsageMetadata{
				PromptTokenCount:     item.Response.Usage.InputTokens,
				CandidatesTokenCount: item.Response.Usage.OutputTokens,
				TotalTokenCount:      item.Response.Usage.TotalTokens,
				PromptTokensDetails:  nil,
				ThoughtsTokenCount:   item.Response.Usage.OutputTokensDetails.ReasoningTokens,
			},
			ModelVersion: c.responseCreated.Response.Model,
			ResponseID:   c.responseCreated.Response.Id,
			Error:        nil,
		},
	}
}

// =============================================================================
// Response Builder
// =============================================================================

func (c *NativeResponseChunkToResponseChunkConverter) buildResponse(parts []Part) Response {
	return Response{
		Candidates: []Candidate{{
			Content: Content{
				Role:  RoleModel,
				Parts: parts,
			},
			FinishReason: "",
		}},
		UsageMetadata: nil,
		ModelVersion:  c.responseCreated.Response.Model,
		ResponseID:    c.responseCreated.Response.Id,
		Error:         nil,
	}
}
