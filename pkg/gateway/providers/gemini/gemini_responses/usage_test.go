package gemini_responses

import "testing"

// TestNativeUsage_ThoughtsCountAsOutput pins Gemini's two quirks against the
// native Usage contract: cachedContentTokenCount is already inside
// promptTokenCount (unlike Anthropic's split counts), and thoughtsTokenCount
// sits outside candidatesTokenCount even though the contract treats reasoning
// as a subset of OutputTokens.
func TestNativeUsage_ThoughtsCountAsOutput(t *testing.T) {
	got := nativeUsage(&UsageMetadata{
		PromptTokenCount:        50000,
		CachedContentTokenCount: 48000,
		CandidatesTokenCount:    400,
		ThoughtsTokenCount:      1100,
		TotalTokenCount:         51500,
	})

	if got.InputTokens != 50000 {
		t.Errorf("InputTokens = %d, want 50000 (cached already included)", got.InputTokens)
	}
	if got.InputTokensDetails.CachedTokens != 48000 {
		t.Errorf("CachedTokens = %d, want 48000", got.InputTokensDetails.CachedTokens)
	}
	if got.OutputTokens != 1500 {
		t.Errorf("OutputTokens = %d, want 1500 (400 candidates + 1100 thoughts)", got.OutputTokens)
	}
	if got.OutputTokensDetails.ReasoningTokens != 1100 {
		t.Errorf("ReasoningTokens = %d, want 1100", got.OutputTokensDetails.ReasoningTokens)
	}

	// TotalTokens == InputTokens + OutputTokens, and it agrees with the
	// totalTokenCount Gemini reported (which counts thoughts).
	if got.TotalTokens != 51500 {
		t.Errorf("TotalTokens = %d, want 51500", got.TotalTokens)
	}
	if got.TotalTokens != got.InputTokens+got.OutputTokens {
		t.Errorf("TotalTokens (%d) != InputTokens+OutputTokens (%d)",
			got.TotalTokens, got.InputTokens+got.OutputTokens)
	}
	if got.InputTokensDetails.CachedTokens > got.InputTokens {
		t.Errorf("CachedTokens (%d) must be a subset of InputTokens (%d)",
			got.InputTokensDetails.CachedTokens, got.InputTokens)
	}
}

func TestNativeUsage_Nil(t *testing.T) {
	if got := nativeUsage(nil); got.TotalTokens != 0 {
		t.Fatalf("nativeUsage(nil) = %+v, want zero value", got)
	}
}
