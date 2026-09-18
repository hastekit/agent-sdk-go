package gemini_responses

type Request struct {
	Model             string            `json:"model"`
	GenerationConfig  *GenerationConfig `json:"generationConfig,omitempty"`
	SystemInstruction *Content          `json:"systemInstruction,omitempty"`
	Contents          []Content         `json:"contents"`
	Tools             []Tool            `json:"tools,omitempty"`
	Stream            *bool             `json:"-"`
}

type GenerationConfig struct {
	MaxOutputTokens    *int            `json:"maxOutputTokens,omitempty"`
	Temperature        *float64        `json:"temperature,omitempty"`
	TopP               *float64        `json:"topP,omitempty"`
	TopK               *int64          `json:"topK,omitempty"`
	ThinkingConfig     *ThinkingConfig `json:"thinkingConfig,omitempty"`
	ResponseModalities []string        `json:"responseModalities"`

	// Structured output
	ResponseMimeType   *string        `json:"responseMimeType,omitempty"`
	ResponseJsonSchema map[string]any `json:"responseJsonSchema,omitempty"`
}

type ThinkingConfig struct {
	IncludeThoughts *bool   `json:"includeThoughts,omitempty"`
	ThinkingBudget  *int    `json:"thinkingBudget,omitempty"`
	ThinkingLevel   *string `json:"thinkingLevel,omitempty"`
}

type Content struct {
	Role  Role   `json:"role,omitempty,omitzero"`
	Parts []Part `json:"parts"`
}

func (s *Content) String() string {
	out := ""
	if s == nil {
		return out
	}

	for _, part := range s.Parts {
		if part.Text != nil && *part.Text != "" {
			out += *part.Text
		}
	}

	return out
}

type Part struct {
	Text *string `json:"text,omitempty"`

	FunctionCall *FunctionCall `json:"functionCall,omitempty"`

	FunctionResponse *FunctionResponse `json:"functionResponse,omitempty"`

	Thought          *bool   `json:"thought,omitempty"`
	ThoughtSignature *string `json:"thoughtSignature,omitempty"`

	InlineData *InlinePartData `json:"inlineData,omitempty"`

	// FileData points at bytes Gemini already holds — a Files API URI, a
	// gs:// object on Vertex — rather than carrying them inline.
	FileData *FilePartData `json:"fileData,omitempty"`

	ExecutableCode      *ExecutableCodePart      `json:"executableCode,omitempty"`
	CodeExecutionResult *CodeExecutionResultPart `json:"codeExecutionResult,omitempty"`
}

type InlinePartData struct {
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"`
}

type FilePartData struct {
	MimeType string `json:"mimeType,omitempty"`
	FileURI  string `json:"fileUri"`
}

func (p *Part) IsThought() bool {
	if p.Thought == nil {
		return false
	}
	return *p.Thought
}

type FunctionCall struct {
	Name string `json:"name"`
	Args any    `json:"args"`
}

type FunctionResponse struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Response map[string]any `json:"response,omitempty"`

	// Parts carries what will not fit in Response, which is JSON and so has
	// nowhere to put an image. A tool that returns a screenshot puts it here.
	Parts []FunctionResponsePart `json:"parts,omitempty"`
}

type FunctionResponsePart struct {
	InlineData *FunctionResponseBlob     `json:"inlineData,omitempty"`
	FileData   *FunctionResponseFileData `json:"fileData,omitempty"`
}

type FunctionResponseBlob struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`

	// DisplayName names the part so the structured Response can point at it
	// with {"$ref": "<displayName>"}, and names it for the model either way.
	DisplayName string `json:"displayName,omitempty"`
}

type FunctionResponseFileData struct {
	MimeType    string `json:"mimeType,omitempty"`
	FileURI     string `json:"fileUri"`
	DisplayName string `json:"displayName,omitempty"`
}

type ExecutableCodePart struct {
	Language string `json:"language"`
	Code     string `json:"code"`
}

type CodeExecutionResultPart struct {
	Outcome string `json:"outcome"` // "OUTCOME_OK"
	Output  string `json:"output"`
}

type Tool struct {
	FunctionDeclarations []FunctionTool     `json:"functionDeclarations,omitempty"`
	CodeExecution        *CodeExecutionTool `json:"code_execution,omitempty"`
}

type FunctionTool struct {
	Name                 string         `json:"name"`
	Description          string         `json:"description"`
	ParametersJsonSchema map[string]any `json:"parametersJsonSchema,omitempty"`
	ResponseJsonSchema   any            `json:"responseJsonSchema,omitempty"`
}

type CodeExecutionTool struct {
}
