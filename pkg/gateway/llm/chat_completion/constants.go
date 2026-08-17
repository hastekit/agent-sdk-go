package chat_completion

import (
	"fmt"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
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

type ContentTypeText string

func (m *ContentTypeText) Value() string                { return "text" }
func (m *ContentTypeText) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m *ContentTypeText) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ContentTypeImageUrl string

func (m *ContentTypeImageUrl) Value() string                { return "image_url" }
func (m *ContentTypeImageUrl) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m *ContentTypeImageUrl) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ContentTypeInputAudio string

func (m *ContentTypeInputAudio) Value() string                { return "input_audio" }
func (m *ContentTypeInputAudio) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m *ContentTypeInputAudio) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ContentTypeFile string

func (m *ContentTypeFile) Value() string                { return "file" }
func (m *ContentTypeFile) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m *ContentTypeFile) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ContentTypeRefusal string

func (m *ContentTypeRefusal) Value() string                { return "refusal" }
func (m *ContentTypeRefusal) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m *ContentTypeRefusal) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

// --------------------- //
// End Of Content Types //
// ------------------- //

// -------------- //
// Tool Call Types //
// -------------//

type ToolCallTypeFunction string

func (m *ToolCallTypeFunction) Value() string                { return "function" }
func (m *ToolCallTypeFunction) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m *ToolCallTypeFunction) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

type ToolCallTypeCustom string

func (m *ToolCallTypeCustom) Value() string                { return "custom" }
func (m *ToolCallTypeCustom) MarshalJSON() ([]byte, error) { return sonic.Marshal(m.Value()) }
func (m *ToolCallTypeCustom) UnmarshalJSON(buf []byte) error {
	return unmarshalConstantString(m, buf)
}

// --------------------- //
// End Of Tool Call Types //
// ------------------- //

// Check if RoleTool exists in constants, if not we'll need to handle it
var RoleTool constants.Role = "tool"
