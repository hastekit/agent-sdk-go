package gemini_responses

type Response struct {
	Candidates    []Candidate    `json:"candidates"`
	UsageMetadata *UsageMetadata `json:"usageMetadata,omitempty"`
	ModelVersion  string         `json:"modelVersion"`
	ResponseID    string         `json:"responseId"`
	Error         *Error         `json:"error,omitempty"`
}

type Candidate struct {
	Content      Content `json:"content"`
	FinishReason string  `json:"finishReason,omitempty"`
}

type UsageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
	PromptTokensDetails  []struct {
		Modality   string `json:"modality"`
		TokenCount int    `json:"tokenCount"`
	} `json:"promptTokensDetails"`
	ThoughtsTokenCount int `json:"thoughtsTokenCount"`
	// CachedContentTokenCount is the cached portion of the prompt. Unlike
	// Anthropic's split counts it is already inside PromptTokenCount, so it
	// maps to InputTokensDetails.CachedTokens and must not be added on top.
	CachedContentTokenCount int `json:"cachedContentTokenCount"`
}

type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}
