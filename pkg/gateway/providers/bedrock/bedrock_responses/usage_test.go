package bedrock_responses

import "testing"

// TestNativeUsage_FoldsCacheTokensIntoPrompt pins the normalization against
// AWS's documented Converse accounting: inputTokens counts only the tokens that
// were neither read from nor written to the cache, so the real prompt is
// inputTokens + cacheReadInputTokens + cacheWriteInputTokens.
//
// The first case uses the shape AWS's own docs show, where totalTokens (600) is
// inputTokens+outputTokens and excludes the 500 cached tokens entirely — which
// is why the reported total cannot be used to infer whether the cache counts
// are already included.
func TestNativeUsage_FoldsCacheTokensIntoPrompt(t *testing.T) {
	tests := []struct {
		name       string
		in         ConverseUsage
		wantInput  int
		wantCached int
		wantTotal  int
	}{
		{
			name: "documented cache-hit shape",
			in: ConverseUsage{
				InputTokens:          550,
				OutputTokens:         50,
				TotalTokens:          600,
				CacheReadInputTokens: 500,
			},
			wantInput:  1050,
			wantCached: 500,
			wantTotal:  1100,
		},
		{
			name: "cache write on first call",
			in: ConverseUsage{
				InputTokens:           500,
				OutputTokens:          300,
				TotalTokens:           800,
				CacheWriteInputTokens: 9500,
			},
			wantInput:  10000,
			wantCached: 0, // a write processed the tokens in full; nothing was served from cache
			wantTotal:  10300,
		},
		{
			name:       "no caching is a no-op",
			in:         ConverseUsage{InputTokens: 1200, OutputTokens: 340, TotalTokens: 1540},
			wantInput:  1200,
			wantCached: 0,
			wantTotal:  1540,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nativeUsage(tt.in)

			if got.InputTokens != tt.wantInput {
				t.Errorf("InputTokens = %d, want %d", got.InputTokens, tt.wantInput)
			}
			if got.InputTokensDetails.CachedTokens != tt.wantCached {
				t.Errorf("CachedTokens = %d, want %d", got.InputTokensDetails.CachedTokens, tt.wantCached)
			}
			if got.TotalTokens != tt.wantTotal {
				t.Errorf("TotalTokens = %d, want %d", got.TotalTokens, tt.wantTotal)
			}
			if got.TotalTokens != got.InputTokens+got.OutputTokens {
				t.Errorf("TotalTokens (%d) != InputTokens+OutputTokens (%d)",
					got.TotalTokens, got.InputTokens+got.OutputTokens)
			}
			if got.InputTokensDetails.CachedTokens > got.InputTokens {
				t.Errorf("CachedTokens (%d) must be a subset of InputTokens (%d)",
					got.InputTokensDetails.CachedTokens, got.InputTokens)
			}
		})
	}
}
