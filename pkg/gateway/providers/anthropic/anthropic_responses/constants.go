package anthropic_responses

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
// Content Types //
// -------------//

type ContentType interface {
}

type ContentTypeText string

func (m ContentTypeText) Value() string                  { return "text" }
func (m ContentTypeText) MarshalJSON() ([]byte, error)   { return sonic.Marshal(m.Value()) }
func (m ContentTypeText) UnmarshalJSON(buf []byte) error { return unmarshalConstantString(m, buf) }

type ContentTypeImage string

func (m ContentTypeImage) Value() string                  { return "image" }
func (m ContentTypeImage) MarshalJSON() ([]byte, error)   { return sonic.Marshal(m.Value()) }
func (m ContentTypeImage) UnmarshalJSON(buf []byte) error { return unmarshalConstantString(m, buf) }

type ContentTypeDocument string

func (m ContentTypeDocument) Value() string                { return "document" }
func (m ContentTypeDocument) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m ContentTypeDocument) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ContentTypeToolUse string

func (m ContentTypeToolUse) Value() string                  { return "tool_use" }
func (m ContentTypeToolUse) MarshalJSON() ([]byte, error)   { return sonic.Marshal(m.Value()) }
func (m ContentTypeToolUse) UnmarshalJSON(buf []byte) error { return unmarshalConstantString(m, buf) }

type ContentTypeToolUseResult string

func (m ContentTypeToolUseResult) Value() string                { return "tool_result" }
func (m ContentTypeToolUseResult) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m ContentTypeToolUseResult) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ContentTypeThinking string

func (m ContentTypeThinking) Value() string                  { return "thinking" }
func (m ContentTypeThinking) MarshalJSON() ([]byte, error)   { return sonic.Marshal(m.Value()) }
func (m ContentTypeThinking) UnmarshalJSON(buf []byte) error { return unmarshalConstantString(m, buf) }

type ContentTypeRedactedThinking string

func (m ContentTypeRedactedThinking) Value() string                { return "redacted_thinking" }
func (m ContentTypeRedactedThinking) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m ContentTypeRedactedThinking) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ContentTypeServerToolUse string

func (m ContentTypeServerToolUse) Value() string                { return "server_tool_use" }
func (m ContentTypeServerToolUse) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m ContentTypeServerToolUse) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ContentTypeWebSearchResultContent string

func (m ContentTypeWebSearchResultContent) Value() string { return "web_search_tool_result" }
func (m ContentTypeWebSearchResultContent) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m ContentTypeWebSearchResultContent) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ContentTypeBashCodeExecutionToolResultContent string

func (m ContentTypeBashCodeExecutionToolResultContent) Value() string {
	return "bash_code_execution_tool_result"
}
func (m ContentTypeBashCodeExecutionToolResultContent) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m ContentTypeBashCodeExecutionToolResultContent) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ContentTypeDeltaText string

func (m ContentTypeDeltaText) Value() string                  { return "text_delta" }
func (m ContentTypeDeltaText) MarshalJSON() ([]byte, error)   { return sonic.Marshal(m.Value()) }
func (m ContentTypeDeltaText) UnmarshalJSON(buf []byte) error { return unmarshalConstantString(m, buf) }

type ContentTypeDeltaInputJSON string

func (m ContentTypeDeltaInputJSON) Value() string                { return "input_json_delta" }
func (m ContentTypeDeltaInputJSON) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m ContentTypeDeltaInputJSON) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ContentTypeDeltaThinking string

func (m ContentTypeDeltaThinking) Value() string                { return "thinking_delta" }
func (m ContentTypeDeltaThinking) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m ContentTypeDeltaThinking) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ContentTypeDeltaThinkingSignature string

func (m ContentTypeDeltaThinkingSignature) Value() string { return "signature_delta" }
func (m ContentTypeDeltaThinkingSignature) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m ContentTypeDeltaThinkingSignature) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ContentTypeDeltaCitation string

func (m ContentTypeDeltaCitation) Value() string { return "citations_delta" }
func (m ContentTypeDeltaCitation) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(m.Value())
}
func (m ContentTypeDeltaCitation) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

// --------------------- //
// End Of Content Types //
// ------------------- //

// -------------- //
// Message Roles //
// -------------//

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// --------------------- //
// End Of Message Roles //
// ------------------- //

// ----------- //
// Chunk Type //
// --------- //

type ChunkTypeMessageStart string

func (m ChunkTypeMessageStart) Value() string                { return "message_start" }
func (m ChunkTypeMessageStart) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m ChunkTypeMessageStart) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeMessageDelta string

func (m ChunkTypeMessageDelta) Value() string                { return "message_delta" }
func (m ChunkTypeMessageDelta) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m ChunkTypeMessageDelta) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeMessageStop string

func (m ChunkTypeMessageStop) Value() string                { return "message_stop" }
func (m ChunkTypeMessageStop) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m ChunkTypeMessageStop) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeContentBlockStart string

func (m ChunkTypeContentBlockStart) Value() string                { return "content_block_start" }
func (m ChunkTypeContentBlockStart) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m ChunkTypeContentBlockStart) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeContentBlockDelta string

func (m ChunkTypeContentBlockDelta) Value() string                { return "content_block_delta" }
func (m ChunkTypeContentBlockDelta) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m ChunkTypeContentBlockDelta) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ChunkTypeContentBlockStop string

func (m ChunkTypeContentBlockStop) Value() string                { return "content_block_stop" }
func (m ChunkTypeContentBlockStop) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m ChunkTypeContentBlockStop) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

// ------------------ //
// End Of Chunk Type //
// ---------------- //

// ------------//
// Tool types //
// ----------//

type ToolTypeCustomTool string

func (m ToolTypeCustomTool) Value() string                { return "custom" }
func (m ToolTypeCustomTool) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m ToolTypeCustomTool) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ToolTypeWebSearchTool string

func (m ToolTypeWebSearchTool) Value() string                { return "web_search_20250305" }
func (m ToolTypeWebSearchTool) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m ToolTypeWebSearchTool) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ToolTypeCodeExecutionTool string

func (m ToolTypeCodeExecutionTool) Value() string                { return "code_execution_20250825" }
func (m ToolTypeCodeExecutionTool) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m ToolTypeCodeExecutionTool) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

// ------------------ //
// End of tool types //
// ---------------- //

// ------------ //
// Stop Reason //
// ---------- //

type StopReason string

const (
	StopReasonEndTurn  StopReason = "end_turn"
	StopReasonMaxToken StopReason = "max_token"
)
