package responses

import (
	"encoding/json"
	"errors"

	"github.com/bytedance/sonic"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/hastekit-sdk-go/pkg/utils"
)

type Request struct {
	Parameters
	Model        string      `json:"model"`
	Input        InputUnion  `json:"input,omitempty,omitzero"`
	Instructions *string     `json:"instructions,omitempty"`
	Tools        []ToolUnion `json:"tools,omitempty"`
}

type Parameters struct {
	Temperature     *float64          `json:"temperature,omitempty"`
	MaxOutputTokens *int              `json:"max_output_tokens,omitempty"`
	TopP            *float64          `json:"top_p,omitempty"`
	TopLogprobs     *int64            `json:"top_logprobs,omitempty"`
	Text            *TextFormat       `json:"text,omitempty"`
	Background      *bool             `json:"background,omitempty"`
	Reasoning       *ReasoningParam   `json:"reasoning,omitempty"`
	Store           *bool             `json:"store,omitempty"`
	Include         []Includable      `json:"include,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
	Stream          *bool             `json:"stream,omitempty"`

	MaxToolCalls      *int  `json:"max_tool_calls,omitempty"`
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`

	ExtraFields map[string]any `json:"extra_fields,omitempty"`
}

type TextFormat struct {
	Format map[string]any `json:"format,omitempty"`
}

func (s *Request) IsStreamingRequest() bool {
	if s.Stream == nil {
		return false
	}

	return *s.Stream
}

type Includable string

const (
	IncludableCodeInterpreterCallOutputs       Includable = "code_interpreter_call.outputs"
	IncludableComputerCallOutputOutputImageURL Includable = "computer_call_output.output.image_url"
	IncludableFileSearchCallResults            Includable = "file_search_call.results"
	IncludableMessageInputImageImageURL        Includable = "message.input_image.image_url"
	IncludableMessageOutputTextLogprobs        Includable = "message.output_text.logprobs"
	IncludableReasoningEncryptedContent        Includable = "reasoning.encrypted_content"
)

type ReasoningParam struct {
	Summary      *string `json:"summary"` // "auto", "concise", "detailed"
	Effort       *string `json:"effort"`  // "none", "low", "medium", "high", "xhigh"
	BudgetTokens *int    `json:",omitempty"`
}

type InputUnion struct {
	OfString           *string          `json:",omitempty,inline"`
	OfInputMessageList InputMessageList `json:",omitempty,inline"`
}

func (u *InputUnion) UnmarshalJSON(data []byte) error {
	var s string
	if err := sonic.Unmarshal(data, &s); err == nil {
		u.OfString = utils.Ptr(s)
		return nil
	}

	var list InputMessageList
	if err := sonic.Unmarshal(data, &list); err == nil {
		u.OfInputMessageList = list
		return nil
	}

	return errors.New("invalid input union")
}

func (u *InputUnion) MarshalJSON() ([]byte, error) {
	if u.OfString != nil {
		return sonic.Marshal(u.OfString)
	}

	if u.OfInputMessageList != nil {
		return sonic.Marshal(u.OfInputMessageList)
	}

	return nil, nil
}

type InputMessageList []InputMessageUnion

type InputMessageUnion struct {
	OfEasyInput                       *EasyMessage                            `json:",omitempty"`
	OfInputMessage                    *InputMessage                           `json:",omitempty"`
	OfOutputMessage                   *OutputMessage                          `json:",omitempty"`
	OfFunctionCall                    *FunctionCallMessage                    `json:",omitempty"`
	OfFunctionCallInterruptResolution *FunctionCallInterruptResolutionMessage `json:",omitempty"`
	OfFunctionCallOutput              *FunctionCallOutputMessage              `json:",omitempty"`
	OfReasoning                       *ReasoningMessage                       `json:",omitempty"`
	OfImageGenerationCall             *ImageGenerationCallMessage             `json:",omitempty,inline"`
	OfWebSearchCall                   *WebSearchCallMessage                   `json:",omitempty,inline"`
	OfCodeInterpreterCall             *CodeInterpreterCallMessage             `json:",omitempty,inline"`
	//OfFileSearchCall       *ResponseFileSearchToolCallParam            `json:",omitempty,inline"`
	//OfComputerCall         *ResponseComputerToolCallParam              `json:",omitempty,inline"`
	//OfComputerCallOutput   *ResponseInputItemComputerCallOutputParam   `json:",omitempty,inline"`
	//OfLocalShellCall       *ResponseInputItemLocalShellCallParam       `json:",omitempty,inline"`
	//OfLocalShellCallOutput *ResponseInputItemLocalShellCallOutputParam `json:",omitempty,inline"`
	//OfMcpListTools         *ResponseInputItemMcpListToolsParam         `json:",omitempty,inline"`
	//OfMcpApprovalRequest   *ResponseInputItemMcpApprovalRequestParam   `json:",omitempty,inline"`
	//OfMcpApprovalResponse  *ResponseInputItemMcpApprovalResponseParam  `json:",omitempty,inline"`
	//OfMcpCall              *ResponseInputItemMcpCallParam              `json:",omitempty,inline"`
	//OfCustomToolCallOutput *ResponseCustomToolCallOutputParam          `json:",omitempty,inline"`
	//OfCustomToolCall       *ResponseCustomToolCallParam                `json:",omitempty,inline"`
	//OfItemReference        *ResponseInputItemItemReferenceParam        `json:",omitempty,inline"`
}

func (u *InputMessageUnion) ID() string {
	if u.OfEasyInput != nil {
		return u.OfEasyInput.ID
	}

	if u.OfInputMessage != nil {
		return u.OfInputMessage.ID
	}

	if u.OfOutputMessage != nil {
		return u.OfOutputMessage.ID
	}

	if u.OfFunctionCall != nil {
		return u.OfFunctionCall.ID
	}

	if u.OfFunctionCallInterruptResolution != nil {
		return u.OfFunctionCallInterruptResolution.ID
	}

	if u.OfFunctionCallOutput != nil {
		return u.OfFunctionCallOutput.ID
	}

	if u.OfReasoning != nil {
		return u.OfReasoning.ID
	}

	if u.OfImageGenerationCall != nil {
		return u.OfImageGenerationCall.ID
	}

	if u.OfWebSearchCall != nil {
		return u.OfWebSearchCall.ID
	}

	if u.OfCodeInterpreterCall != nil {
		return u.OfCodeInterpreterCall.ID
	}

	return ""
}

func (u *InputMessageUnion) UnmarshalJSON(data []byte) error {
	var easyMessage EasyMessage
	if err := sonic.Unmarshal(data, &easyMessage); err == nil {
		u.OfEasyInput = &easyMessage
		return nil
	}

	var textMessage InputMessage
	if err := sonic.Unmarshal(data, &textMessage); err == nil {
		u.OfInputMessage = &textMessage
		return nil
	}

	var outputMessage OutputMessage
	if err := sonic.Unmarshal(data, &outputMessage); err == nil {
		u.OfOutputMessage = &outputMessage
		return nil
	}

	var fnCallMsg FunctionCallMessage
	if err := sonic.Unmarshal(data, &fnCallMsg); err == nil {
		u.OfFunctionCall = &fnCallMsg
		return nil
	}

	var fnCallInterruptResolution FunctionCallInterruptResolutionMessage
	if err := sonic.Unmarshal(data, &fnCallInterruptResolution); err == nil {
		u.OfFunctionCallInterruptResolution = &fnCallInterruptResolution
		return nil
	}

	var fnCallOutputMsg FunctionCallOutputMessage
	if err := sonic.Unmarshal(data, &fnCallOutputMsg); err == nil {
		u.OfFunctionCallOutput = &fnCallOutputMsg
		return nil
	}

	var reasoningMsg ReasoningMessage
	if err := sonic.Unmarshal(data, &reasoningMsg); err == nil {
		u.OfReasoning = &reasoningMsg
		return nil
	}

	var imageGenerationCallMsg ImageGenerationCallMessage
	if err := sonic.Unmarshal(data, &imageGenerationCallMsg); err == nil {
		u.OfImageGenerationCall = &imageGenerationCallMsg
		return nil
	}

	var webSearchCallMsg WebSearchCallMessage
	if err := sonic.Unmarshal(data, &webSearchCallMsg); err == nil {
		u.OfWebSearchCall = &webSearchCallMsg
		return nil
	}

	var codeInterpreterMsg CodeInterpreterCallMessage
	if err := sonic.Unmarshal(data, &codeInterpreterMsg); err == nil {
		u.OfCodeInterpreterCall = &codeInterpreterMsg
		return nil
	}

	return errors.New("invalid input message union")
}

func (u *InputMessageUnion) MarshalJSON() ([]byte, error) {
	if u.OfEasyInput != nil {
		return sonic.Marshal(u.OfEasyInput)
	}

	if u.OfInputMessage != nil {
		return sonic.Marshal(u.OfInputMessage)
	}

	if u.OfOutputMessage != nil {
		return sonic.Marshal(u.OfOutputMessage)
	}

	if u.OfFunctionCall != nil {
		return sonic.Marshal(u.OfFunctionCall)
	}

	if u.OfFunctionCallInterruptResolution != nil {
		return sonic.Marshal(u.OfFunctionCallInterruptResolution)
	}

	if u.OfFunctionCallOutput != nil {
		return sonic.Marshal(u.OfFunctionCallOutput)
	}

	if u.OfReasoning != nil {
		return sonic.Marshal(u.OfReasoning)
	}

	if u.OfImageGenerationCall != nil {
		return sonic.Marshal(u.OfImageGenerationCall)
	}

	if u.OfWebSearchCall != nil {
		return sonic.Marshal(u.OfWebSearchCall)
	}

	if u.OfCodeInterpreterCall != nil {
		return sonic.Marshal(u.OfCodeInterpreterCall)
	}

	return nil, nil
}

type EasyMessage struct {
	Type    constants.MessageTypeMessage `json:"type"` // Always "message".
	ID      string                       `json:"id,omitempty"`
	Role    constants.Role               `json:"role,omitempty"`
	Content EasyInputContentUnion        `json:"content"`
}

type InputMessage struct {
	Type    constants.MessageTypeMessage `json:"type"` // Always "message".
	ID      string                       `json:"id,omitempty"`
	Role    constants.Role               `json:"role,omitempty"` // Any of "user", "system", "developer".
	Content InputContent                 `json:"content,omitempty"`
}

type FunctionCallMessage struct {
	Type             constants.MessageTypeFunctionCall `json:"type"`
	ID               string                            `json:"id,omitempty"`
	CallID           string                            `json:"call_id,omitempty"`
	Name             string                            `json:"name"`
	Arguments        string                            `json:"arguments"`
	ThoughtSignature *string                           `json:"thought_signature,omitempty"` // Only for gemini
}

// Interrupt resolution actions. v1 supports approve/reject; accept/decline/
// cancel are reserved for future data-carrying modes.
const (
	InterruptActionApprove = "approve"
	InterruptActionReject  = "reject"
)

// InterruptResolution is one user decision for a paused interrupt. Action
// is the verb the user took (InterruptActionApprove / InterruptActionReject).
// Content carries mode-specific response data (e.g. a filled form for a
// structured/form elicitation); on resume the agent loop delivers the whole
// resolution — Content included — to the resuming tool via its
// ToolCall.ResumeMessages, so a tool that raised a data interrupt reads the
// user's answer from there.
// RememberAction turns a one-off answer into a standing one: the tool this
// call names is added to the thread's always-allow list on approve, or its
// always-deny list on reject, and the same tool is not asked about again on
// this thread. It is the "don't ask me again" / "never do this" checkbox next
// to the approve and reject buttons.
type InterruptResolution struct {
	CallID         string          `json:"call_id"`
	Action         string          `json:"action"`
	Content        json.RawMessage `json:"content,omitempty"`
	RememberAction bool            `json:"remember_action,omitempty"`
}

// FunctionCallInterruptResolutionMessage is the resume message for the
// Interrupt path — the inbound counterpart of a paused run's interrupts.
// The agent loop drains it into the QueuedApprovals/QueuedRejections
// queues in v1 (approve/reject actions).
type FunctionCallInterruptResolutionMessage struct {
	Type        constants.MessageTypeFunctionCallInterruptResolution `json:"type"`
	ID          string                                               `json:"id"`
	Resolutions []InterruptResolution                                `json:"resolutions"`
}

type FunctionCallOutputMessage struct {
	Type   constants.MessageTypeFunctionCallOutput `json:"type"`
	ID     string                                  `json:"id,omitempty"`
	CallID string                                  `json:"call_id"`
	Output FunctionCallOutputContentUnion          `json:"output"`
}

type ReasoningMessage struct {
	Type             constants.MessageTypeReasoning `json:"type"`
	ID               string                         `json:"id,omitempty"`
	Summary          []SummaryTextContent           `json:"summary"`
	EncryptedContent *string                        `json:"encrypted_content,omitempty"`
}

type ImageGenerationCallMessage struct {
	Type         constants.MessageTypeImageGenerationCall `json:"type"`
	ID           string                                   `json:"id"`            // "ig_" prefixed
	Status       string                                   `json:"status"`        // "generating"
	Background   string                                   `json:"background"`    // "opaque"
	OutputFormat string                                   `json:"output_format"` // "png"
	Quality      string                                   `json:"quality"`       // "medium"
	Size         string                                   `json:"size"`          // 100x100
	Result       string                                   `json:"result"`        // Base64 image
}

type WebSearchCallMessage struct {
	Type   constants.MessageTypeWebSearchCall `json:"type"`
	ID     string                             `json:"id"`
	Action WebSearchCallActionUnion           `json:"action"`
	Status string                             `json:"status"` // "in_progress", "searching", "completed", "failed"
}

type CodeInterpreterCallMessage struct {
	Type        constants.MessageTypeCodeInterpreterCall `json:"type"`
	ID          string                                   `json:"id"`
	Status      string                                   `json:"status"`
	Code        string                                   `json:"code"`
	ContainerID string                                   `json:"container_id"`
	Outputs     []CodeInterpreterCallOutputParam         `json:"outputs,omitempty"`
}

type CodeInterpreterCallOutputParam struct {
	Type string `json:"type"` // "logs"
	Logs string `json:"logs"`
}

type EasyInputContentUnion struct {
	OfString           *string      `json:",omitempty"`
	OfInputMessageList InputContent `json:",omitempty"`
}

func (u *EasyInputContentUnion) UnmarshalJSON(data []byte) error {
	var s string
	if err := sonic.Unmarshal(data, &s); err == nil {
		u.OfString = &s
		return nil
	}

	var inputContent InputContent
	if err := sonic.Unmarshal(data, &inputContent); err == nil {
		u.OfInputMessageList = inputContent
		return nil
	}

	return errors.New("invalid easy input content union")
}

func (u *EasyInputContentUnion) MarshalJSON() ([]byte, error) {
	if u.OfString != nil {
		return sonic.Marshal(u.OfString)
	}

	if u.OfInputMessageList != nil {
		return sonic.Marshal(u.OfInputMessageList)
	}

	return nil, nil
}

type InputContent []InputContentUnion

type InputContentUnion struct {
	OfInputText  *InputTextContent  `json:",omitempty"`
	OfOutputText *OutputTextContent `json:",omitempty"`
	OfInputImage *InputImageContent `json:",omitempty,inline"`
	OfInputFile  *InputFileContent  `json:",omitempty,inline"`
}

func (u *InputContentUnion) UnmarshalJSON(data []byte) error {
	var textContext InputTextContent
	if err := sonic.Unmarshal(data, &textContext); err == nil {
		u.OfInputText = &textContext
		return nil
	}

	var outputTextContent OutputTextContent
	if err := sonic.Unmarshal(data, &outputTextContent); err == nil {
		u.OfOutputText = &outputTextContent

		// We will force annotations to exist
		if outputTextContent.Annotations == nil {
			outputTextContent.Annotations = []Annotation{}
		}
		return nil
	}

	var inputImageContent InputImageContent
	if err := sonic.Unmarshal(data, &inputImageContent); err == nil {
		u.OfInputImage = &inputImageContent
		return nil
	}

	var inputFileContent InputFileContent
	if err := sonic.Unmarshal(data, &inputFileContent); err == nil {
		u.OfInputFile = &inputFileContent
		return nil
	}

	return errors.New("invalid input content union")
}

func (u *InputContentUnion) MarshalJSON() ([]byte, error) {
	if u.OfInputText != nil {
		return sonic.Marshal(u.OfInputText)
	}

	if u.OfOutputText != nil {
		return sonic.Marshal(u.OfOutputText)
	}

	if u.OfInputImage != nil {
		return sonic.Marshal(u.OfInputImage)
	}

	if u.OfInputFile != nil {
		return sonic.Marshal(u.OfInputFile)
	}

	return nil, nil
}

type InputTextContent struct {
	Type constants.ContentTypeInputText `json:"type"`
	Text string                         `json:"text"`
}

type OutputTextContent struct {
	Type        constants.ContentTypeOutputText `json:"type"`
	Text        string                          `json:"text"`
	Annotations []Annotation                    `json:"annotations"`
}

type ReasoningTextContent struct {
	Type constants.ContentTypeReasoningText `json:"type"`
	Text string                             `json:"text"`
}

type InputImageContent struct {
	Type     constants.ContentTypeInputImage `json:"type"`
	ImageURL *string                         `json:"image_url,omitempty"` // A fully qualified URL or base64 encoded image in a data URL.
	FileID   *string                         `json:"file_id,omitempty"`
	Detail   string                          `json:"detail"` // "low", "high", "auto"
}

type InputFileContent struct {
	Type     constants.ContentTypeInputFile `json:"type"`
	FileURL  *string                        `json:"file_url,omitempty"`
	FileName *string                        `json:"filename,omitempty"`
	FileData *string                        `json:"file_data,omitempty"`
	FileID   *string                        `json:"file_id,omitempty"`
}

type SummaryTextContent struct {
	Type constants.ContentTypeSummaryText `json:"type"`
	Text string                           `json:"text"`
}

type Annotation struct {
	Type       string `json:"type"` // Any of "file_citation", "url_citation", "container_file_citation", "file_path".
	Title      string `json:"title"`
	URL        string `json:"url"`
	StartIndex int    `json:"start_index"`
	EndIndex   int    `json:"end_index"`

	ExtraParams map[string]any `json:"extra_params"`
}

type FunctionCallOutputContentUnion struct {
	OfString *string      `json:",omitempty"`
	OfList   InputContent `json:",omitempty"`
}

func (u *FunctionCallOutputContentUnion) UnmarshalJSON(data []byte) error {
	var s string
	if err := sonic.Unmarshal(data, &s); err == nil {
		u.OfString = &s
		return nil
	}

	var inputContent InputContent
	if err := sonic.Unmarshal(data, &inputContent); err == nil {
		u.OfList = inputContent
		return nil
	}

	return errors.New("invalid function call output content union")
}

func (u *FunctionCallOutputContentUnion) MarshalJSON() ([]byte, error) {
	if u.OfString != nil {
		return sonic.Marshal(u.OfString)
	}

	if u.OfList != nil {
		return sonic.Marshal(u.OfList)
	}

	return nil, nil
}

type ToolUnion struct {
	OfFunction        *FunctionTool        `json:",omitempty"`
	OfImageGeneration *ImageGenerationTool `json:",omitempty"`
	OfWebSearch       *WebSearchTool       `json:",omitempty"`
	OfCodeExecution   *CodeExecutionTool   `json:",omitempty"`
}

func (u *ToolUnion) UnmarshalJSON(data []byte) error {
	var fnTool FunctionTool
	if err := sonic.Unmarshal(data, &fnTool); err == nil {
		u.OfFunction = &fnTool
		return nil
	}

	var imageGenerationTool ImageGenerationTool
	if err := sonic.Unmarshal(data, &imageGenerationTool); err == nil {
		u.OfImageGeneration = &imageGenerationTool
		return nil
	}

	var webSearchTool WebSearchTool
	if err := sonic.Unmarshal(data, &webSearchTool); err == nil {
		u.OfWebSearch = &webSearchTool
		return nil
	}

	var codeExecutionTool CodeExecutionTool
	if err := sonic.Unmarshal(data, &codeExecutionTool); err == nil {
		u.OfCodeExecution = &codeExecutionTool
		return nil
	}

	return errors.New("invalid tool union")
}

func (u *ToolUnion) MarshalJSON() ([]byte, error) {
	if u.OfFunction != nil {
		return sonic.Marshal(u.OfFunction)
	}

	if u.OfImageGeneration != nil {
		return sonic.Marshal(u.OfImageGeneration)
	}

	if u.OfWebSearch != nil {
		return sonic.Marshal(u.OfWebSearch)
	}

	if u.OfCodeExecution != nil {
		return sonic.Marshal(u.OfCodeExecution)
	}

	return nil, nil
}

type FunctionTool struct {
	Type        constants.ToolTypeFunction `json:"type"` // "function"
	Name        string                     `json:"name"`
	Description *string                    `json:"description,omitempty"`
	Parameters  map[string]any             `json:"parameters,omitempty"`
	Strict      *bool                      `json:"strict,omitempty"`
}

type ImageGenerationTool struct {
	Type    constants.ToolTypeImageGeneration `json:"type"` // image_generation
	Size    string                            `json:"size"`
	Quality string                            `json:"quality"`
}

type WebSearchTool struct {
	Type              constants.ToolTypeWebSearch `json:"type"` // web_search
	Filters           *WebSearchToolFilters       `json:"filters,omitempty"`
	UserLocation      *WebSearchToolUserLocation  `json:"user_location,omitempty"`
	ExternalWebAccess *bool                       `json:"external_web_access,omitempty"`
	SearchContextSize *string                     `json:"search_context_size,omitempty"` // "low", "medium", "high" - "medium" is default
}

type WebSearchToolFilters struct {
	AllowedDomains []string `json:"allowed_domains"`
}

type WebSearchToolUserLocation struct {
	Type     string `json:"type"`
	Country  string `json:"country"`
	City     string `json:"city"`
	Region   string `json:"region"`
	Timezone string `json:"timezone"`
}

type WebSearchCallActionUnion struct {
	OfSearch   *WebSearchCallActionOfSearch   `json:",omitempty"`
	OfOpenPage *WebSearchCallActionOfOpenPage `json:",omitempty"`
	OfFind     *WebSearchCallActionOfFind     `json:",omitempty"`
	OfString   *string                        `json:",omitempty"`
}

func (u *WebSearchCallActionUnion) UnmarshalJSON(data []byte) error {
	var searchAction WebSearchCallActionOfSearch
	if err := sonic.Unmarshal(data, &searchAction); err == nil {
		u.OfSearch = &searchAction
		return nil
	}

	var openPageAction WebSearchCallActionOfOpenPage
	if err := sonic.Unmarshal(data, &openPageAction); err == nil {
		u.OfOpenPage = &openPageAction
		return nil
	}

	var findAction WebSearchCallActionOfFind
	if err := sonic.Unmarshal(data, &findAction); err == nil {
		u.OfFind = &findAction
		return nil
	}

	var str string
	if err := sonic.Unmarshal(data, &str); err == nil {
		u.OfString = &str
		return nil
	}

	return errors.New("invalid web search call action union")
}

func (u *WebSearchCallActionUnion) MarshalJSON() ([]byte, error) {
	if u.OfSearch != nil {
		return sonic.Marshal(u.OfSearch)
	}

	if u.OfOpenPage != nil {
		return sonic.Marshal(u.OfOpenPage)
	}

	if u.OfFind != nil {
		return sonic.Marshal(u.OfFind)
	}

	if u.OfString != nil {
		return sonic.Marshal(u.OfString)
	}

	return nil, nil
}

type WebSearchCallActionOfSearch struct {
	Type    constants.WebSearchActionTypeSearch `json:"type"`
	Queries []string                            `json:"queries"`
	Query   string                              `json:"query"`
	Sources []WebSearchCallActionOfSearchSource `json:"sources"`
}

type WebSearchCallActionOfOpenPage struct {
	Type constants.WebSearchActionTypeOpenPage `json:"type"`
	URL  string                                `json:"url"`
}

type WebSearchCallActionOfFind struct {
	Type    constants.WebSearchActionTypeFind `json:"type"`
	URL     string                            `json:"url"`
	Pattern string                            `json:"pattern"`
}

type WebSearchCallActionOfSearchSource struct {
	Type        string         `json:"type"` // always "url"
	URL         string         `json:"url"`
	ExtraParams map[string]any `json:"extra_params"`
}

type CodeExecutionTool struct {
	Type      constants.ToolTypeCodeExecution  `json:"type"` // code_interpreter
	Container *CodeExecutionToolContainerUnion `json:"container,omitempty"`
}

type CodeExecutionToolContainerUnion struct {
	ContainerID     *string                           `json:",omitempty"`
	ContainerConfig *CodeExecutionToolContainerConfig `json:",omitempty"`
}

func (u *CodeExecutionToolContainerUnion) UnmarshalJSON(data []byte) error {
	var containerId string
	if err := sonic.Unmarshal(data, &containerId); err == nil {
		u.ContainerID = &containerId
		return nil
	}

	var containerConfig CodeExecutionToolContainerConfig
	if err := sonic.Unmarshal(data, &containerConfig); err == nil {
		u.ContainerConfig = &containerConfig
		return nil
	}

	return errors.New("invalid code execution tool container union")
}

func (u *CodeExecutionToolContainerUnion) MarshalJSON() ([]byte, error) {
	if u.ContainerID != nil {
		return sonic.Marshal(u.ContainerID)
	}

	if u.ContainerConfig != nil {
		return sonic.Marshal(u.ContainerConfig)
	}

	return nil, nil
}

type CodeExecutionToolContainerConfig struct {
	Type        string   `json:"type"` // "auto"
	FileIds     []string `json:"file_ids"`
	MemoryLimit string   `json:"memory_limit"`
}
