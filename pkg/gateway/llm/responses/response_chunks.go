package responses

import (
	"errors"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// --------------------//
// Chunk definitions //
// -----------------//

type ResponseChunk struct {
	OfResponseCreated    *ChunkResponse[constants.ChunkTypeResponseCreated]    `json:",omitempty"`
	OfResponseInProgress *ChunkResponse[constants.ChunkTypeResponseInProgress] `json:",omitempty"`
	OfResponseCompleted  *ChunkResponse[constants.ChunkTypeResponseCompleted]  `json:",omitempty"`

	OfOutputItemAdded *ChunkOutputItem[constants.ChunkTypeOutputItemAdded] `json:",omitempty"`
	OfOutputItemDone  *ChunkOutputItem[constants.ChunkTypeOutputItemDone]  `json:",omitempty"`

	// For output item of type "message"
	OfContentPartAdded          *ChunkContentPart[constants.ChunkTypeContentPartAdded]         `json:",omitempty"`
	OfContentPartDone           *ChunkContentPart[constants.ChunkTypeContentPartDone]          `json:",omitempty"`
	OfOutputTextDelta           *ChunkOutputText[constants.ChunkTypeOutputTextDelta]           `json:",omitempty"`
	OfOutputTextAnnotationAdded *ChunkOutputText[constants.ChunkTypeOutputTextAnnotationAdded] `json:",omitempty"`
	OfOutputTextDone            *ChunkOutputText[constants.ChunkTypeOutputTextDone]            `json:",omitempty"`

	// For output item of type "function_call"
	OfFunctionCallArgumentsDelta *ChunkFunctionCall[constants.ChunkTypeFunctionCallArgumentsDelta] `json:",omitempty"`
	OfFunctionCallArgumentsDone  *ChunkFunctionCall[constants.ChunkTypeFunctionCallArgumentsDone]  `json:",omitempty"`

	// For output item of type "reasoning"
	OfReasoningSummaryPartAdded *ChunkReasoningSummaryPart[constants.ChunkTypeReasoningSummaryPartAdded] `json:",omitempty"`
	OfReasoningSummaryPartDone  *ChunkReasoningSummaryPart[constants.ChunkTypeReasoningSummaryPartDone]  `json:",omitempty"`
	OfReasoningSummaryTextDelta *ChunkReasoningSummaryText[constants.ChunkTypeReasoningSummaryTextDelta] `json:",omitempty"`
	OfReasoningSummaryTextDone  *ChunkReasoningSummaryText[constants.ChunkTypeReasoningSummaryTextDone]  `json:",omitempty"`

	OfReasoningTextDelta *ChunkReasoningText[constants.ChunkTypeReasoningTextDelta]    `json:",omitempty"` // Only for OSS models
	OfReasoningTextDone  *ChunkReasoningTextDone[constants.ChunkTypeReasoningTextDone] `json:",omitempty"` // Only for OSS models

	// For output item of type "image_generation_call"
	OfImageGenerationCallInProgress   *ChunkImageGenerationCall[constants.ChunkTypeImageGenerationCallInProgress]   `json:",omitempty"`
	OfImageGenerationCallGenerating   *ChunkImageGenerationCall[constants.ChunkTypeImageGenerationCallGenerating]   `json:",omitempty"`
	OfImageGenerationCallPartialImage *ChunkImageGenerationCall[constants.ChunkTypeImageGenerationCallPartialImage] `json:",omitempty"`

	// For output item of type "web_search_call"
	OfWebSearchCallInProgress *ChunkWebSearchCall[constants.ChunkTypeWebSearchCallInProgress] `json:",omitempty"`
	OfWebSearchCallSearching  *ChunkWebSearchCall[constants.ChunkTypeWebSearchCallSearching]  `json:",omitempty"`
	OfWebSearchCallCompleted  *ChunkWebSearchCall[constants.ChunkTypeWebSearchCallCompleted]  `json:",omitempty"`

	// For output item of type "code_interpreter"
	OfCodeInterpreterCallInProgress   *ChunkCodeInterpreterCall[constants.ChunkTypeCodeInterpreterCallInProgress]   `json:",omitempty"`
	OfCodeInterpreterCallCodeDelta    *ChunkCodeInterpreterCall[constants.ChunkTypeCodeInterpreterCallCodeDelta]    `json:",omitempty"`
	OfCodeInterpreterCallCodeDone     *ChunkCodeInterpreterCall[constants.ChunkTypeCodeInterpreterCallCodeDone]     `json:",omitempty"`
	OfCodeInterpreterCallInterpreting *ChunkCodeInterpreterCall[constants.ChunkTypeCodeInterpreterCallInterpreting] `json:",omitempty"`
	OfCodeInterpreterCallCompleted    *ChunkCodeInterpreterCall[constants.ChunkTypeCodeInterpreterCallCompleted]    `json:",omitempty"`

	// Custom Chunks
	OfRunCreated         *ChunkRun[constants.ChunkTypeRunCreated]    `json:",omitempty"`
	OfRunInProgress      *ChunkRun[constants.ChunkTypeRunInProgress] `json:",omitempty"`
	OfRunPaused          *ChunkRun[constants.ChunkTypeRunPaused]     `json:",omitempty"`
	OfRunCompleted       *ChunkRun[constants.ChunkTypeRunCompleted]  `json:",omitempty"`
	OfFunctionCallOutput *FunctionCallOutputMessage                  `json:",omitempty"`

	// OfToolProgress carries a mid-execution progress update emitted by a
	// running tool call (function tool, MCP tool, etc.). It is a live,
	// best-effort side stream keyed by CallID — it never enters history and
	// is not replayed under durable runtimes. For MCP tools it is the
	// SDK-native projection of the server's notifications/progress.
	OfToolProgress *ChunkToolProgress[constants.ChunkTypeToolProgress] `json:",omitempty"`
}

func (u *ResponseChunk) UnmarshalJSON(data []byte) error {
	var toolProgress *ChunkToolProgress[constants.ChunkTypeToolProgress]
	if err := sonic.Unmarshal(data, &toolProgress); err == nil {
		u.OfToolProgress = toolProgress
		return nil
	}

	var runCreated *ChunkRun[constants.ChunkTypeRunCreated]
	if err := sonic.Unmarshal(data, &runCreated); err == nil {
		u.OfRunCreated = runCreated
		return nil
	}

	var runInProgress *ChunkRun[constants.ChunkTypeRunInProgress]
	if err := sonic.Unmarshal(data, &runInProgress); err == nil {
		u.OfRunInProgress = runInProgress
		return nil
	}

	var runPaused *ChunkRun[constants.ChunkTypeRunPaused]
	if err := sonic.Unmarshal(data, &runPaused); err == nil {
		u.OfRunPaused = runPaused
		return nil
	}

	var runCompleted *ChunkRun[constants.ChunkTypeRunCompleted]
	if err := sonic.Unmarshal(data, &runCompleted); err == nil {
		u.OfRunCompleted = runCompleted
		return nil
	}

	var responseCreated *ChunkResponse[constants.ChunkTypeResponseCreated]
	if err := sonic.Unmarshal(data, &responseCreated); err == nil {
		u.OfResponseCreated = responseCreated
		return nil
	}

	var functionCallOutput *FunctionCallOutputMessage
	if err := sonic.Unmarshal(data, &functionCallOutput); err == nil {
		u.OfFunctionCallOutput = functionCallOutput
		return nil
	}

	var responseInProgress *ChunkResponse[constants.ChunkTypeResponseInProgress]
	if err := sonic.Unmarshal(data, &responseInProgress); err == nil {
		u.OfResponseInProgress = responseInProgress
		return nil
	}

	var responseCompleted *ChunkResponse[constants.ChunkTypeResponseCompleted]
	if err := sonic.Unmarshal(data, &responseCompleted); err == nil {
		u.OfResponseCompleted = responseCompleted
		return nil
	}

	var outputItemAdded *ChunkOutputItem[constants.ChunkTypeOutputItemAdded]
	if err := sonic.Unmarshal(data, &outputItemAdded); err == nil {
		u.OfOutputItemAdded = outputItemAdded
		return nil
	}

	var outputItemDone *ChunkOutputItem[constants.ChunkTypeOutputItemDone]
	if err := sonic.Unmarshal(data, &outputItemDone); err == nil {
		u.OfOutputItemDone = outputItemDone
		return nil
	}

	var contentPartAdded *ChunkContentPart[constants.ChunkTypeContentPartAdded]
	if err := sonic.Unmarshal(data, &contentPartAdded); err == nil {
		u.OfContentPartAdded = contentPartAdded
		return nil
	}

	var contentPartDone *ChunkContentPart[constants.ChunkTypeContentPartDone]
	if err := sonic.Unmarshal(data, &contentPartDone); err == nil {
		u.OfContentPartDone = contentPartDone
		return nil
	}

	var outputTextDelta *ChunkOutputText[constants.ChunkTypeOutputTextDelta]
	if err := sonic.Unmarshal(data, &outputTextDelta); err == nil {
		u.OfOutputTextDelta = outputTextDelta
		return nil
	}

	var outputTextAnnotationAdded *ChunkOutputText[constants.ChunkTypeOutputTextAnnotationAdded]
	if err := sonic.Unmarshal(data, &outputTextAnnotationAdded); err == nil {
		u.OfOutputTextAnnotationAdded = outputTextAnnotationAdded
		return nil
	}

	var outputTextDone *ChunkOutputText[constants.ChunkTypeOutputTextDone]
	if err := sonic.Unmarshal(data, &outputTextDone); err == nil {
		u.OfOutputTextDone = outputTextDone
		return nil
	}

	var fnCall *ChunkFunctionCall[constants.ChunkTypeFunctionCallArgumentsDelta]
	if err := sonic.Unmarshal(data, &fnCall); err == nil {
		u.OfFunctionCallArgumentsDelta = fnCall
		return nil
	}

	var fnCallDone *ChunkFunctionCall[constants.ChunkTypeFunctionCallArgumentsDone]
	if err := sonic.Unmarshal(data, &fnCallDone); err == nil {
		u.OfFunctionCallArgumentsDone = fnCallDone
		return nil
	}

	var reasoningTextDelta *ChunkReasoningText[constants.ChunkTypeReasoningTextDelta]
	if err := sonic.Unmarshal(data, &reasoningTextDelta); err == nil {
		u.OfReasoningTextDelta = reasoningTextDelta
		return nil
	}

	var reasoningTextDone *ChunkReasoningTextDone[constants.ChunkTypeReasoningTextDone]
	if err := sonic.Unmarshal(data, &reasoningTextDone); err == nil {
		u.OfReasoningTextDone = reasoningTextDone
		return nil
	}

	var reasoningSummaryPartAdded *ChunkReasoningSummaryPart[constants.ChunkTypeReasoningSummaryPartAdded]
	if err := sonic.Unmarshal(data, &reasoningSummaryPartAdded); err == nil {
		u.OfReasoningSummaryPartAdded = reasoningSummaryPartAdded
		return nil
	}

	var reasoningSummaryPartDone *ChunkReasoningSummaryPart[constants.ChunkTypeReasoningSummaryPartDone]
	if err := sonic.Unmarshal(data, &reasoningSummaryPartDone); err == nil {
		u.OfReasoningSummaryPartDone = reasoningSummaryPartDone
		return nil
	}

	var reasoningSummaryTextDelta *ChunkReasoningSummaryText[constants.ChunkTypeReasoningSummaryTextDelta]
	if err := sonic.Unmarshal(data, &reasoningSummaryTextDelta); err == nil {
		u.OfReasoningSummaryTextDelta = reasoningSummaryTextDelta
		return nil
	}

	var reasoningSummaryTextDone *ChunkReasoningSummaryText[constants.ChunkTypeReasoningSummaryTextDone]
	if err := sonic.Unmarshal(data, &reasoningSummaryTextDone); err == nil {
		u.OfReasoningSummaryTextDone = reasoningSummaryTextDone
		return nil
	}

	var imageGenerationCallInProgress *ChunkImageGenerationCall[constants.ChunkTypeImageGenerationCallInProgress]
	if err := sonic.Unmarshal(data, &imageGenerationCallInProgress); err == nil {
		u.OfImageGenerationCallInProgress = imageGenerationCallInProgress
		return nil
	}

	var imageGenerationCallGenerating *ChunkImageGenerationCall[constants.ChunkTypeImageGenerationCallGenerating]
	if err := sonic.Unmarshal(data, &imageGenerationCallGenerating); err == nil {
		u.OfImageGenerationCallGenerating = imageGenerationCallGenerating
		return nil
	}

	var imageGenerationCallPartialImage *ChunkImageGenerationCall[constants.ChunkTypeImageGenerationCallPartialImage]
	if err := sonic.Unmarshal(data, &imageGenerationCallPartialImage); err == nil {
		u.OfImageGenerationCallPartialImage = imageGenerationCallPartialImage
		return nil
	}

	var webSearchCallInProgress *ChunkWebSearchCall[constants.ChunkTypeWebSearchCallInProgress]
	if err := sonic.Unmarshal(data, &webSearchCallInProgress); err == nil {
		u.OfWebSearchCallInProgress = webSearchCallInProgress
		return nil
	}

	var webSearchCallSearching *ChunkWebSearchCall[constants.ChunkTypeWebSearchCallSearching]
	if err := sonic.Unmarshal(data, &webSearchCallSearching); err == nil {
		u.OfWebSearchCallSearching = webSearchCallSearching
		return nil
	}

	var webSearchCallCompleted *ChunkWebSearchCall[constants.ChunkTypeWebSearchCallCompleted]
	if err := sonic.Unmarshal(data, &webSearchCallCompleted); err == nil {
		u.OfWebSearchCallCompleted = webSearchCallCompleted
		return nil
	}

	var codeInterpreterCallInProgress *ChunkCodeInterpreterCall[constants.ChunkTypeCodeInterpreterCallInProgress]
	if err := sonic.Unmarshal(data, &codeInterpreterCallInProgress); err == nil {
		u.OfCodeInterpreterCallInProgress = codeInterpreterCallInProgress
		return nil
	}

	var codeInterpreterCallCodeDelta *ChunkCodeInterpreterCall[constants.ChunkTypeCodeInterpreterCallCodeDelta]
	if err := sonic.Unmarshal(data, &codeInterpreterCallCodeDelta); err == nil {
		u.OfCodeInterpreterCallCodeDelta = codeInterpreterCallCodeDelta
		return nil
	}

	var codeInterpreterCallCodeDone *ChunkCodeInterpreterCall[constants.ChunkTypeCodeInterpreterCallCodeDone]
	if err := sonic.Unmarshal(data, &codeInterpreterCallCodeDone); err == nil {
		u.OfCodeInterpreterCallCodeDone = codeInterpreterCallCodeDone
		return nil
	}

	var codeInterpreterCallInterpreting *ChunkCodeInterpreterCall[constants.ChunkTypeCodeInterpreterCallInterpreting]
	if err := sonic.Unmarshal(data, &codeInterpreterCallInterpreting); err == nil {
		u.OfCodeInterpreterCallInterpreting = codeInterpreterCallInterpreting
		return nil
	}

	var codeInterpreterCallCompleted *ChunkCodeInterpreterCall[constants.ChunkTypeCodeInterpreterCallCompleted]
	if err := sonic.Unmarshal(data, &codeInterpreterCallCompleted); err == nil {
		u.OfCodeInterpreterCallCompleted = codeInterpreterCallCompleted
		return nil
	}

	return errors.New("invalid response chunk union")
}

func (u *ResponseChunk) MarshalJSON() ([]byte, error) {
	if u.OfResponseCreated != nil {
		return sonic.Marshal(u.OfResponseCreated)
	}

	if u.OfResponseInProgress != nil {
		return sonic.Marshal(u.OfResponseInProgress)
	}

	if u.OfResponseCompleted != nil {
		return sonic.Marshal(u.OfResponseCompleted)
	}

	if u.OfOutputItemAdded != nil {
		return sonic.Marshal(u.OfOutputItemAdded)
	}

	if u.OfOutputItemDone != nil {
		return sonic.Marshal(u.OfOutputItemDone)
	}

	if u.OfContentPartAdded != nil {
		return sonic.Marshal(u.OfContentPartAdded)
	}

	if u.OfContentPartDone != nil {
		return sonic.Marshal(u.OfContentPartDone)
	}

	if u.OfOutputTextDelta != nil {
		return sonic.Marshal(u.OfOutputTextDelta)
	}

	if u.OfOutputTextAnnotationAdded != nil {
		return sonic.Marshal(u.OfOutputTextAnnotationAdded)
	}

	if u.OfOutputTextDone != nil {
		return sonic.Marshal(u.OfOutputTextDone)
	}

	if u.OfFunctionCallArgumentsDelta != nil {
		return sonic.Marshal(u.OfFunctionCallArgumentsDelta)
	}

	if u.OfFunctionCallArgumentsDone != nil {
		return sonic.Marshal(u.OfFunctionCallArgumentsDone)
	}

	if u.OfReasoningSummaryPartAdded != nil {
		return sonic.Marshal(u.OfReasoningSummaryPartAdded)
	}

	if u.OfReasoningSummaryPartDone != nil {
		return sonic.Marshal(u.OfReasoningSummaryPartDone)
	}

	if u.OfReasoningSummaryTextDelta != nil {
		return sonic.Marshal(u.OfReasoningSummaryTextDelta)
	}

	if u.OfReasoningTextDelta != nil {
		return sonic.Marshal(u.OfReasoningTextDelta)
	}

	if u.OfReasoningTextDone != nil {
		return sonic.Marshal(u.OfReasoningTextDone)
	}

	if u.OfImageGenerationCallInProgress != nil {
		return sonic.Marshal(u.OfImageGenerationCallInProgress)
	}

	if u.OfImageGenerationCallGenerating != nil {
		return sonic.Marshal(u.OfImageGenerationCallGenerating)
	}

	if u.OfImageGenerationCallPartialImage != nil {
		return sonic.Marshal(u.OfImageGenerationCallPartialImage)
	}

	if u.OfReasoningSummaryTextDone != nil {
		return sonic.Marshal(u.OfReasoningSummaryTextDone)
	}

	if u.OfWebSearchCallInProgress != nil {
		return sonic.Marshal(u.OfWebSearchCallInProgress)
	}

	if u.OfWebSearchCallSearching != nil {
		return sonic.Marshal(u.OfWebSearchCallSearching)
	}

	if u.OfWebSearchCallCompleted != nil {
		return sonic.Marshal(u.OfWebSearchCallCompleted)
	}

	// Custom Chunks
	if u.OfRunCreated != nil {
		return sonic.Marshal(u.OfRunCreated)
	}

	if u.OfRunInProgress != nil {
		return sonic.Marshal(u.OfRunInProgress)
	}

	if u.OfRunPaused != nil {
		return sonic.Marshal(u.OfRunPaused)
	}

	if u.OfRunCompleted != nil {
		return sonic.Marshal(u.OfRunCompleted)
	}

	if u.OfFunctionCallOutput != nil {
		return sonic.Marshal(u.OfFunctionCallOutput)
	}

	if u.OfToolProgress != nil {
		return sonic.Marshal(u.OfToolProgress)
	}

	if u.OfCodeInterpreterCallInProgress != nil {
		return sonic.Marshal(u.OfCodeInterpreterCallInProgress)
	}

	if u.OfCodeInterpreterCallCodeDelta != nil {
		return sonic.Marshal(u.OfCodeInterpreterCallCodeDelta)
	}

	if u.OfCodeInterpreterCallCodeDone != nil {
		return sonic.Marshal(u.OfCodeInterpreterCallCodeDone)
	}

	if u.OfCodeInterpreterCallInterpreting != nil {
		return sonic.Marshal(u.OfCodeInterpreterCallInterpreting)
	}

	if u.OfCodeInterpreterCallCompleted != nil {
		return sonic.Marshal(u.OfCodeInterpreterCallCompleted)
	}

	return nil, nil
}

func (u *ResponseChunk) ChunkType() string {
	if u.OfResponseCreated != nil {
		return u.OfResponseCreated.Type.Value()
	}

	if u.OfResponseInProgress != nil {
		return u.OfResponseInProgress.Type.Value()
	}

	if u.OfResponseCompleted != nil {
		return u.OfResponseCompleted.Type.Value()
	}

	if u.OfOutputItemAdded != nil {
		return u.OfOutputItemAdded.Type.Value()
	}

	if u.OfOutputItemDone != nil {
		return u.OfOutputItemDone.Type.Value()
	}

	if u.OfContentPartAdded != nil {
		return u.OfContentPartAdded.Type.Value()
	}

	if u.OfContentPartDone != nil {
		return u.OfContentPartDone.Type.Value()
	}

	if u.OfOutputTextDelta != nil {
		return u.OfOutputTextDelta.Type.Value()
	}

	if u.OfOutputTextAnnotationAdded != nil {
		return u.OfOutputTextAnnotationAdded.Type.Value()
	}

	if u.OfOutputTextDone != nil {
		return u.OfOutputTextDone.Type.Value()
	}

	if u.OfFunctionCallArgumentsDelta != nil {
		return u.OfFunctionCallArgumentsDelta.Type.Value()
	}

	if u.OfFunctionCallArgumentsDone != nil {
		return u.OfFunctionCallArgumentsDone.Type.Value()
	}

	if u.OfReasoningSummaryPartAdded != nil {
		return u.OfReasoningSummaryPartAdded.Type.Value()
	}

	if u.OfReasoningSummaryPartDone != nil {
		return u.OfReasoningSummaryPartDone.Type.Value()
	}

	if u.OfReasoningSummaryTextDelta != nil {
		return u.OfReasoningSummaryTextDelta.Type.Value()
	}

	if u.OfReasoningSummaryTextDone != nil {
		return u.OfReasoningSummaryTextDone.Type.Value()
	}

	if u.OfReasoningTextDelta != nil {
		return u.OfReasoningTextDelta.Type.Value()
	}

	if u.OfReasoningTextDone != nil {
		return u.OfReasoningTextDone.Type.Value()
	}

	if u.OfImageGenerationCallInProgress != nil {
		return u.OfImageGenerationCallInProgress.Type.Value()
	}

	if u.OfImageGenerationCallGenerating != nil {
		return u.OfImageGenerationCallGenerating.Type.Value()
	}

	if u.OfImageGenerationCallPartialImage != nil {
		return u.OfImageGenerationCallPartialImage.Type.Value()
	}

	if u.OfWebSearchCallInProgress != nil {
		return u.OfWebSearchCallInProgress.Type.Value()
	}

	if u.OfWebSearchCallSearching != nil {
		return u.OfWebSearchCallSearching.Type.Value()
	}

	if u.OfWebSearchCallCompleted != nil {
		return u.OfWebSearchCallCompleted.Type.Value()
	}

	if u.OfCodeInterpreterCallInProgress != nil {
		return u.OfCodeInterpreterCallInProgress.Type.Value()
	}

	if u.OfCodeInterpreterCallCodeDelta != nil {
		return u.OfCodeInterpreterCallCodeDelta.Type.Value()
	}

	if u.OfCodeInterpreterCallCodeDone != nil {
		return u.OfCodeInterpreterCallCodeDone.Type.Value()
	}

	if u.OfCodeInterpreterCallInterpreting != nil {
		return u.OfCodeInterpreterCallInterpreting.Type.Value()
	}

	if u.OfCodeInterpreterCallCompleted != nil {
		return u.OfCodeInterpreterCallCompleted.Type.Value()
	}

	// Custom Chunks
	if u.OfRunCreated != nil {
		return u.OfRunCreated.Type.Value()
	}

	if u.OfRunInProgress != nil {
		return u.OfRunInProgress.Type.Value()
	}

	if u.OfRunPaused != nil {
		return u.OfRunPaused.Type.Value()
	}

	if u.OfRunCompleted != nil {
		return u.OfRunCompleted.Type.Value()
	}

	if u.OfFunctionCallOutput != nil {
		return u.OfFunctionCallOutput.Type.Value()
	}

	if u.OfToolProgress != nil {
		return u.OfToolProgress.Type.Value()
	}

	return ""
}

type ChunkRun[T any] struct {
	Type           T            `json:"type"`
	SequenceNumber int          `json:"sequence_number"`
	RunState       ChunkRunData `json:"run_state"`
}

// ChunkToolProgress is a mid-execution progress update for a single tool
// call. Its shape mirrors MCP's notifications/progress
// (progress/total/message) so an MCP tool's server notifications map onto it
// losslessly, while function tools emit the same shape via the
// ProgressReporter injected into their ToolCall. CallID ties the update back
// to the in-flight function call; Progress should increase monotonically and
// Total is optional (0 = unknown).
type ChunkToolProgress[T any] struct {
	Type           T       `json:"type"`
	SequenceNumber int     `json:"sequence_number"`
	CallID         string  `json:"call_id"`
	ToolName       string  `json:"tool_name,omitempty"`
	Progress       float64 `json:"progress"`
	Total          float64 `json:"total,omitempty"`
	Message        string  `json:"message,omitempty"`
}

type ChunkRunData struct {
	Id                string      `json:"id"`
	Object            string      `json:"object"` // "run"
	Status            string      `json:"status"` // "created", "in_progress", "paused", "resumed", "completed", "aborted"
	PendingInterrupts []Interrupt `json:"pending_interrupts,omitempty"`
	Usage             Usage       `json:"usage"`
	TraceID           string      `json:"traceid"`
}

// InterruptMode discriminates the kind of tool-call intercept a run is
// paused on. It lets a single Interrupt carry orchestrator HITL,
// MCP URL elicitation, and future intercept types under one shape.
type InterruptMode string

const (
	// InterruptModeApproval is a human approve/reject gate (workflow
	// approval nodes and tool-level RequiresApproval).
	InterruptModeApproval InterruptMode = "approval"
	// InterruptModeURL is an MCP URL elicitation — the user must visit a
	// URL (e.g. an OAuth connect flow) before the tool can proceed.
	InterruptModeURL InterruptMode = "url"
	// InterruptModeForm is a structured (form) elicitation: the user fills
	// a form described by a JSON schema (carried in the Elicitations'
	// RequestedSchema). The filled form returns as the resolution Content,
	// which the agent loop delivers to the resuming tool via its
	// ToolCall.ResumeMessages.
	InterruptModeForm InterruptMode = "form"
)

// Interrupt is the unified representation of one paused tool call awaiting
// user input. FunctionCallMessage identifies the paused call (and, for
// approval mode, carries the name/args the user is approving). Mode-
// specific payload lives in Elicitations (URL/form modes). IsNested is
// true when the interrupt was raised inside a nested tool call (e.g. an
// inner agent's MCP tool) and bubbled up to the top-level run.
type Interrupt struct {
	FunctionCallMessage FunctionCallMessage `json:"function_call_message"`
	Mode                InterruptMode       `json:"mode"`
	Elicitations        []mcp.ElicitParams  `json:"elicitations,omitempty"`
	IsNested            bool                `json:"is_nested,omitempty"`
}

type ChunkResponse[T any] struct {
	Type           T                 `json:"type"`
	SequenceNumber int               `json:"sequence_number"`
	Response       ChunkResponseData `json:"response"`
}

type ChunkResponseData struct {
	Id                string               `json:"id"`
	Object            string               `json:"object"`
	CreatedAt         int                  `json:"created_at"`
	Status            string               `json:"status"`
	Background        bool                 `json:"background"`
	Error             interface{}          `json:"error"`
	IncompleteDetails interface{}          `json:"incomplete_details"`
	Output            []OutputMessageUnion `json:"output"`
	Usage             Usage                `json:"usage"`
	Request
}

type ChunkOutputItem[T any] struct {
	Type           T                   `json:"type"`
	SequenceNumber int                 `json:"sequence_number"`
	OutputIndex    int                 `json:"output_index"`
	Item           ChunkOutputItemData `json:"item"`
}

type ChunkOutputItemData struct {
	Type string `json:"type"` // "function_call", "message", "reasoning", "image_generation_call", "web_search_call", "code_interpreter_call"

	// Common fields
	Id     string `json:"id"`
	Status string `json:"status,omitempty"`

	// For output_item of type "message" and "reasoning" (oss)
	Content *ChunkOutputItemContent `json:"content,omitempty"`
	Role    constants.Role          `json:"role,omitempty"`

	// For output_item of type "function_call"
	CallID           *string `json:"call_id,omitempty"`
	Name             *string `json:"name,omitempty"`
	Arguments        *string `json:"arguments,omitempty"`
	ThoughtSignature *string `json:"thought_signature,omitempty"` // Exception: Gemini function calls can have though signature

	// For "reasoning"
	EncryptedContent *string               `json:"encrypted_content,omitempty"`
	Summary          *[]SummaryTextContent `json:"summary,omitempty"`

	// For "image_generation_call"
	Background   *string `json:"background,omitempty"`    // "opaque"
	OutputFormat *string `json:"output_format,omitempty"` // "png"
	Quality      *string `json:"quality,omitempty"`       // "medium"
	Result       *string `json:"result,omitempty"`        // base64 image
	Size         *string `json:"size,omitempty"`          // "1024x1024"

	// For "web_search_call"
	Action *WebSearchCallActionUnion `json:"action,omitempty"`

	// For "code_interpreter_call"
	Code        *string                          `json:"code,omitempty"`
	ContainerID *string                          `json:"container_id,omitempty"`
	Outputs     []CodeInterpreterCallOutputParam `json:"outputs,omitempty"`
}

type ChunkOutputItemContent []ChunkOutputItemContentUnion

type ChunkOutputItemContentUnion struct {
	OfOutputText    *OutputTextContent    `json:",omitempty,inline"`
	OfReasoningText *ReasoningTextContent `json:",omitempty,inline"`
}

func (u *ChunkOutputItemContentUnion) UnmarshalJSON(data []byte) error {
	var textContext OutputTextContent
	if err := sonic.Unmarshal(data, &textContext); err == nil {
		u.OfOutputText = &textContext
		return nil
	}

	var reasoningTextContent ReasoningTextContent
	if err := sonic.Unmarshal(data, &reasoningTextContent); err == nil {
		u.OfReasoningText = &reasoningTextContent
		return nil
	}

	return errors.New("invalid input content union")
}

func (u *ChunkOutputItemContentUnion) MarshalJSON() ([]byte, error) {
	if u.OfOutputText != nil {
		return sonic.Marshal(u.OfOutputText)
	}

	if u.OfReasoningText != nil {
		return sonic.Marshal(u.OfReasoningText)
	}

	return nil, nil
}

type ChunkContentPart[T any] struct {
	Type           T                           `json:"type"`
	SequenceNumber int                         `json:"sequence_number"`
	ItemId         string                      `json:"item_id"`
	OutputIndex    int                         `json:"output_index"`
	ContentIndex   int                         `json:"content_index"`
	Part           ChunkOutputItemContentUnion `json:"part"`
}

type ChunkOutputText[T any] struct {
	Type           T             `json:"type"`
	SequenceNumber int           `json:"sequence_number"`
	ItemId         string        `json:"item_id"`
	OutputIndex    int           `json:"output_index"`
	ContentIndex   int           `json:"content_index"`
	Delta          string        `json:"delta"`
	Logprobs       []interface{} `json:"logprobs"`
	Obfuscation    string        `json:"obfuscation"`

	// Only on content.output_text.done (contains the accumulated content)
	Text *string `json:"text,omitempty"`

	// Only on response.output_text.annotation.added
	Annotation      *Annotation `json:"annotation,omitempty"`
	AnnotationIndex int         `json:"annotation_index"`
}

type ChunkReasoningText[T any] struct {
	Type           T      `json:"type"`
	SequenceNumber int    `json:"sequence_number"`
	ItemId         string `json:"item_id"`
	OutputIndex    int    `json:"output_index"`
	ContentIndex   int    `json:"content_index"`
	Delta          string `json:"delta"`
}

type ChunkReasoningTextDone[T any] struct {
	Type           T      `json:"type"`
	OutputIndex    int    `json:"output_index"`
	ContentIndex   int    `json:"content_index"`
	Text           string `json:"text"`
	SequenceNumber int    `json:"sequence_number"`
	ItemId         string `json:"item_id"`
}

type ChunkFunctionCall[T any] struct {
	Type T `json:"type"`

	SequenceNumber int    `json:"sequence_number"`
	ItemId         string `json:"item_id"`
	OutputIndex    int    `json:"output_index"`
	Delta          string `json:"delta"`
	Arguments      string `json:"arguments"`
	Obfuscation    string `json:"obfuscation"`
}

type ChunkReasoningSummaryPart[T any] struct {
	Type           T                  `json:"type"`
	SequenceNumber int                `json:"sequence_number"`
	ItemId         string             `json:"item_id"`
	OutputIndex    int                `json:"output_index"`
	SummaryIndex   int                `json:"summary_index"`
	Part           SummaryTextContent `json:"part"`
}

type ChunkReasoningSummaryText[T any] struct {
	Type           T      `json:"type"`
	SequenceNumber int    `json:"sequence_number"`
	ItemId         string `json:"item_id"`
	OutputIndex    int    `json:"output_index"`
	SummaryIndex   int    `json:"summary_index"`

	// Only on response.reasoning_summary_text.delta
	Delta string `json:"delta"`

	// Only on response.reasoning_summary_text.done
	Text *string `json:"text,omitempty"`

	// Helpers - Anthropic sends signature as a separate delta with different type "signature_delta"
	// We would have been using "Delta" for storing the reasoning summary delta, and since we need another field
	// to store the signature, we use this field.
	// We don't use "Text" to avoid confusion.
	EncryptedContent *string `json:"encrypted_content,omitempty"`
}

type ChunkImageGenerationCall[T any] struct {
	Type T `json:"type"`

	SequenceNumber int    `json:"sequence_number"`
	ItemId         string `json:"item_id"`
	OutputIndex    int    `json:"output_index"`

	// Only on response.image_generation_call.partial_image
	PartialImageIndex  int     `json:"partial_image_index"`
	PartialImageBase64 string  `json:"partial_image_b64"`
	Background         *string `json:"background,omitempty"`    // "opaque"
	OutputFormat       *string `json:"output_format,omitempty"` // "png"
	Quality            *string `json:"quality,omitempty"`       // "medium"
	Size               *string `json:"size,omitempty"`          // "1024x1024"
}

type ChunkWebSearchCall[T any] struct {
	Type T `json:"type"`

	SequenceNumber int    `json:"sequence_number"`
	ItemId         string `json:"item_id"`
	OutputIndex    int    `json:"output_index"`
}

type ChunkCodeInterpreterCall[T any] struct {
	Type T `json:"type"`

	ItemId         string `json:"item_id"`
	SequenceNumber int    `json:"sequence_number"`
	OutputIndex    int    `json:"output_index"`

	// Only on response.code_interpreter_call_code.delta
	Delta       *string `json:"delta,omitempty"`
	Obfuscation *string `json:"obfuscation,omitempty"`

	// Only on response.code_interpreter_call_code.done
	Code             *string `json:"code,omitempty"`
	ThoughtSignature *string `json:"thought_signature,omitempty"` // Only for Gemini
}

type ChunkResponseUsage struct {
	InputTokens        int `json:"input_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokens        int `json:"output_tokens"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	TotalTokens int `json:"total_tokens"`
}
