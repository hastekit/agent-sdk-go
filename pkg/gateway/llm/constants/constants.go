package constants

import (
	"fmt"

	"github.com/bytedance/sonic"
)

type StringConstant interface {
	Value() string
}

func unmarshalConstantString(c StringConstant, buf []byte) error {
	var s string
	if err := sonic.Unmarshal(buf, &s); err != nil {
		return err
	}

	if s != c.Value() {
		return fmt.Errorf("invalid %T: got %q, want %q", c, s, c.Value())
	}

	return nil
}

// -------------- //
// Message Types //
// -------------//

type MessageTypeMessage string

func (m *MessageTypeMessage) Value() string { return "message" }
func (m MessageTypeMessage) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *MessageTypeMessage) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type MessageTypeFunctionCall string

func (m *MessageTypeFunctionCall) Value() string               { return "function_call" }
func (m MessageTypeFunctionCall) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m *MessageTypeFunctionCall) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type MessageTypeFunctionCallInterruptResolution string

func (m *MessageTypeFunctionCallInterruptResolution) Value() string {
	return "function_call_interrupt_resolution"
}
func (m MessageTypeFunctionCallInterruptResolution) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *MessageTypeFunctionCallInterruptResolution) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type MessageTypeFunctionCallOutput string

func (m *MessageTypeFunctionCallOutput) Value() string { return "function_call_output" }
func (m MessageTypeFunctionCallOutput) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *MessageTypeFunctionCallOutput) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type MessageTypeReasoning string

func (m *MessageTypeReasoning) Value() string               { return "reasoning" }
func (m MessageTypeReasoning) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m *MessageTypeReasoning) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type MessageTypeImageGenerationCall string

func (m *MessageTypeImageGenerationCall) Value() string { return "image_generation_call" }
func (m MessageTypeImageGenerationCall) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *MessageTypeImageGenerationCall) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type MessageTypeWebSearchCall string

func (m *MessageTypeWebSearchCall) Value() string { return "web_search_call" }
func (m MessageTypeWebSearchCall) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *MessageTypeWebSearchCall) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type MessageTypeCodeInterpreterCall string

func (m *MessageTypeCodeInterpreterCall) Value() string { return "code_interpreter_call" }
func (m MessageTypeCodeInterpreterCall) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *MessageTypeCodeInterpreterCall) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

// --------------------- //
// End Of Message Types //
// ------------------- //

// -------------- //
// Message Roles //
// -------------//

type Role string

const (
	RoleUser      Role = "user"
	RoleDeveloper Role = "developer"
	RoleSystem    Role = "system"
	RoleAssistant Role = "assistant"
)

// --------------------- //
// End Of Message Roles //
// ------------------- //

// -------------- //
// Content Types //
// -------------//

type ContentType interface {
	ContentTypeInputText | ContentTypeOutputText
}

type ContentTypeInputText string

func (m *ContentTypeInputText) Value() string               { return "input_text" }
func (m ContentTypeInputText) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m *ContentTypeInputText) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ContentTypeOutputText string

func (m *ContentTypeOutputText) Value() string               { return "output_text" }
func (m ContentTypeOutputText) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m *ContentTypeOutputText) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ContentTypeReasoningText string

func (m *ContentTypeReasoningText) Value() string { return "reasoning_text" }

type ContentTypeInputImage string

func (m *ContentTypeInputImage) Value() string               { return "input_image" }
func (m ContentTypeInputImage) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m *ContentTypeInputImage) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ContentTypeInputFile string

func (m *ContentTypeInputFile) Value() string               { return "input_file" }
func (m ContentTypeInputFile) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m *ContentTypeInputFile) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ContentTypeSummaryText string

func (m *ContentTypeSummaryText) Value() string               { return "summary_text" }
func (m ContentTypeSummaryText) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m *ContentTypeSummaryText) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

// --------------------- //
// End Of Content Types //
// ------------------- //

// ----------- //
// Chunk Type //
// --------- //

type ChunkTypeRunCreated string

func (m *ChunkTypeRunCreated) Value() string               { return "run.created" }
func (m ChunkTypeRunCreated) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m *ChunkTypeRunCreated) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeRunInProgress string

func (m *ChunkTypeRunInProgress) Value() string               { return "run.in_progress" }
func (m ChunkTypeRunInProgress) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m *ChunkTypeRunInProgress) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeRunPaused string

func (m *ChunkTypeRunPaused) Value() string               { return "run.paused" }
func (m ChunkTypeRunPaused) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m *ChunkTypeRunPaused) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeRunCompleted string

func (m *ChunkTypeRunCompleted) Value() string               { return "run.completed" }
func (m ChunkTypeRunCompleted) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m *ChunkTypeRunCompleted) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeToolProgress string

func (m *ChunkTypeToolProgress) Value() string               { return "tool.progress" }
func (m ChunkTypeToolProgress) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m *ChunkTypeToolProgress) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeBackgroundTaskStarted string

func (m *ChunkTypeBackgroundTaskStarted) Value() string { return "background_task.started" }
func (m ChunkTypeBackgroundTaskStarted) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *ChunkTypeBackgroundTaskStarted) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeBackgroundTaskCompleted string

func (m *ChunkTypeBackgroundTaskCompleted) Value() string { return "background_task.completed" }
func (m ChunkTypeBackgroundTaskCompleted) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *ChunkTypeBackgroundTaskCompleted) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeResponseCreated string

func (m *ChunkTypeResponseCreated) Value() string               { return "response.created" }
func (m ChunkTypeResponseCreated) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m *ChunkTypeResponseCreated) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeResponseInProgress string

func (m *ChunkTypeResponseInProgress) Value() string               { return "response.in_progress" }
func (m ChunkTypeResponseInProgress) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m *ChunkTypeResponseInProgress) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeResponseCompleted string

func (m *ChunkTypeResponseCompleted) Value() string               { return "response.completed" }
func (m ChunkTypeResponseCompleted) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m *ChunkTypeResponseCompleted) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeOutputItemAdded string

func (m *ChunkTypeOutputItemAdded) Value() string               { return "response.output_item.added" }
func (m ChunkTypeOutputItemAdded) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m *ChunkTypeOutputItemAdded) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeOutputItemDone string

func (m *ChunkTypeOutputItemDone) Value() string               { return "response.output_item.done" }
func (m ChunkTypeOutputItemDone) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m *ChunkTypeOutputItemDone) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

// For output item of type "message"

type ChunkTypeContentPartAdded string

func (m *ChunkTypeContentPartAdded) Value() string               { return "response.content_part.added" }
func (m ChunkTypeContentPartAdded) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m *ChunkTypeContentPartAdded) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeContentPartDone string

func (m *ChunkTypeContentPartDone) Value() string               { return "response.content_part.done" }
func (m ChunkTypeContentPartDone) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m *ChunkTypeContentPartDone) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeOutputTextDelta string

func (m *ChunkTypeOutputTextDelta) Value() string               { return "response.output_text.delta" }
func (m ChunkTypeOutputTextDelta) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m *ChunkTypeOutputTextDelta) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeOutputTextAnnotationAdded string

func (m *ChunkTypeOutputTextAnnotationAdded) Value() string {
	return "response.output_text.annotation.added"
}
func (m ChunkTypeOutputTextAnnotationAdded) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *ChunkTypeOutputTextAnnotationAdded) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeOutputTextDone string

func (m *ChunkTypeOutputTextDone) Value() string               { return "response.output_text.done" }
func (m ChunkTypeOutputTextDone) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m *ChunkTypeOutputTextDone) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

// For output item of type "function_call"

type ChunkTypeFunctionCallArgumentsDelta string

func (m *ChunkTypeFunctionCallArgumentsDelta) Value() string {
	return "response.function_call_arguments.delta"
}
func (m ChunkTypeFunctionCallArgumentsDelta) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *ChunkTypeFunctionCallArgumentsDelta) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeFunctionCallArgumentsDone string

func (m *ChunkTypeFunctionCallArgumentsDone) Value() string {
	return "response.function_call_arguments.done"
}
func (m ChunkTypeFunctionCallArgumentsDone) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *ChunkTypeFunctionCallArgumentsDone) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

// For output item of type "reasoning summary"

type ChunkTypeReasoningSummaryPartAdded string

func (m *ChunkTypeReasoningSummaryPartAdded) Value() string {
	return "response.reasoning_summary_part.added"
}
func (m ChunkTypeReasoningSummaryPartAdded) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *ChunkTypeReasoningSummaryPartAdded) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeReasoningSummaryPartDone string

func (m *ChunkTypeReasoningSummaryPartDone) Value() string {
	return "response.reasoning_summary_part.done"
}
func (m ChunkTypeReasoningSummaryPartDone) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *ChunkTypeReasoningSummaryPartDone) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeReasoningSummaryTextDelta string

func (m *ChunkTypeReasoningSummaryTextDelta) Value() string {
	return "response.reasoning_summary_text.delta"
}
func (m ChunkTypeReasoningSummaryTextDelta) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *ChunkTypeReasoningSummaryTextDelta) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeReasoningSummaryTextDone string

func (m *ChunkTypeReasoningSummaryTextDone) Value() string {
	return "response.reasoning_summary_text.done"
}
func (m ChunkTypeReasoningSummaryTextDone) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *ChunkTypeReasoningSummaryTextDone) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

// For output item of type "reasoning text" (applicable for gpt oss models)

type ChunkTypeReasoningTextDelta string

func (m *ChunkTypeReasoningTextDelta) Value() string {
	return "response.reasoning_text.delta"
}
func (m ChunkTypeReasoningTextDelta) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *ChunkTypeReasoningTextDelta) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeReasoningTextDone string

func (m *ChunkTypeReasoningTextDone) Value() string {
	return "response.reasoning_text.done"
}
func (m ChunkTypeReasoningTextDone) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *ChunkTypeReasoningTextDone) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

// For output item of type "image_generation_call"

type ChunkTypeImageGenerationCallInProgress string

func (m *ChunkTypeImageGenerationCallInProgress) Value() string {
	return "response.image_generation_call.in_progress"
}
func (m ChunkTypeImageGenerationCallInProgress) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *ChunkTypeImageGenerationCallInProgress) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeImageGenerationCallGenerating string

func (m *ChunkTypeImageGenerationCallGenerating) Value() string {
	return "response.image_generation_call.generating"
}
func (m ChunkTypeImageGenerationCallGenerating) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *ChunkTypeImageGenerationCallGenerating) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeImageGenerationCallPartialImage string

func (m *ChunkTypeImageGenerationCallPartialImage) Value() string {
	return "response.image_generation_call.partial_image"
}
func (m ChunkTypeImageGenerationCallPartialImage) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *ChunkTypeImageGenerationCallPartialImage) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeWebSearchCallInProgress string

func (m *ChunkTypeWebSearchCallInProgress) Value() string {
	return "response.web_search_call.in_progress"
}
func (m ChunkTypeWebSearchCallInProgress) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *ChunkTypeWebSearchCallInProgress) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeWebSearchCallSearching string

func (m *ChunkTypeWebSearchCallSearching) Value() string {
	return "response.web_search_call.searching"
}
func (m ChunkTypeWebSearchCallSearching) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *ChunkTypeWebSearchCallSearching) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeWebSearchCallCompleted string

func (m *ChunkTypeWebSearchCallCompleted) Value() string {
	return "response.web_search_call.completed"
}
func (m ChunkTypeWebSearchCallCompleted) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *ChunkTypeWebSearchCallCompleted) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeCodeInterpreterCallInProgress string

func (m *ChunkTypeCodeInterpreterCallInProgress) Value() string {
	return "response.code_interpreter_call.in_progress"
}
func (m ChunkTypeCodeInterpreterCallInProgress) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *ChunkTypeCodeInterpreterCallInProgress) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeCodeInterpreterCallCodeDelta string

func (m *ChunkTypeCodeInterpreterCallCodeDelta) Value() string {
	return "response.code_interpreter_call_code.delta"
}
func (m ChunkTypeCodeInterpreterCallCodeDelta) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *ChunkTypeCodeInterpreterCallCodeDelta) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeCodeInterpreterCallCodeDone string

func (m *ChunkTypeCodeInterpreterCallCodeDone) Value() string {
	return "response.code_interpreter_call_code.done"
}
func (m ChunkTypeCodeInterpreterCallCodeDone) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *ChunkTypeCodeInterpreterCallCodeDone) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeCodeInterpreterCallInterpreting string

func (m *ChunkTypeCodeInterpreterCallInterpreting) Value() string {
	return "response.code_interpreter_call.interpreting"
}
func (m ChunkTypeCodeInterpreterCallInterpreting) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *ChunkTypeCodeInterpreterCallInterpreting) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeCodeInterpreterCallCompleted string

func (m *ChunkTypeCodeInterpreterCallCompleted) Value() string {
	return "response.code_interpreter_call.completed"
}
func (m ChunkTypeCodeInterpreterCallCompleted) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *ChunkTypeCodeInterpreterCallCompleted) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

// ------------------ //
// End Of Chunk Type //
// ---------------- //

// -------------- //
// Function Type //
// ------------ //

type ToolTypeFunction string

func (m *ToolTypeFunction) Value() string {
	return "function"
}
func (m ToolTypeFunction) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *ToolTypeFunction) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ToolTypeImageGeneration string

func (m *ToolTypeImageGeneration) Value() string {
	return "image_generation"
}
func (m ToolTypeImageGeneration) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *ToolTypeImageGeneration) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ToolTypeWebSearch string

func (m *ToolTypeWebSearch) Value() string {
	return "web_search"
}
func (m ToolTypeWebSearch) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *ToolTypeWebSearch) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ToolTypeCodeExecution string

func (m *ToolTypeCodeExecution) Value() string {
	return "code_interpreter"
}
func (m ToolTypeCodeExecution) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *ToolTypeCodeExecution) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

// --------------------- //
// End Of Function Type //
// ------------------- //

// ------------------ //
// Web Search Action //
// ---------------- //

type WebSearchActionTypeSearch string

func (m *WebSearchActionTypeSearch) Value() string {
	return "search"
}
func (m WebSearchActionTypeSearch) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *WebSearchActionTypeSearch) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type WebSearchActionTypeOpenPage string

func (m *WebSearchActionTypeOpenPage) Value() string {
	return "open_page"
}
func (m WebSearchActionTypeOpenPage) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *WebSearchActionTypeOpenPage) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type WebSearchActionTypeFind string

func (m *WebSearchActionTypeFind) Value() string {
	return "find"
}
func (m WebSearchActionTypeFind) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m *WebSearchActionTypeFind) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

// ------------------------- //
// End Of Web search action //
// ----------------------- //
