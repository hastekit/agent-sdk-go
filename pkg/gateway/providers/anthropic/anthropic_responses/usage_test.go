package anthropic_responses

import (
	"testing"

	responses2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// TestUsageRoundTrip checks the two directions agree: normalizing an Anthropic
// payload inward and converting it back out must preserve the prompt size.
// The previous inverse wrote CachedTokens into both cache_creation and
// cache_read while also emitting the full InputTokens, so a client summing the
// three fields — as Anthropic's own accounting does — saw the cached tokens
// three times over.
func TestUsageRoundTrip(t *testing.T) {
	native := nativeUsage(&ChunkMessageUsage{
		InputTokens:          500,
		CacheReadInputTokens: 170000,
		OutputTokens:         300,
	})

	wire := anthropicUsage(native)
	if wire == nil {
		t.Fatal("anthropicUsage() = nil")
	}

	// Anthropic's wire contract: the prompt is the sum of the three fields.
	if sum := wire.InputTokens + wire.CacheReadInputTokens + wire.CacheCreationInputTokens; sum != 170500 {
		t.Errorf("wire prompt total = %d, want 170500", sum)
	}
	if wire.InputTokens != 500 {
		t.Errorf("wire InputTokens = %d, want 500 (uncached remainder only)", wire.InputTokens)
	}
	if wire.CacheReadInputTokens != 170000 {
		t.Errorf("wire CacheReadInputTokens = %d, want 170000", wire.CacheReadInputTokens)
	}

	back := nativeUsage(wire)
	if back.InputTokens != native.InputTokens || back.TotalTokens != native.TotalTokens {
		t.Errorf("round trip changed usage: got %+v, want %+v", back, native)
	}
}

// TestAnthropicUsage_CachedExceedsInput guards the degenerate case where a
// provider violates the subset rule, so the split never emits a negative
// input_tokens.
func TestAnthropicUsage_CachedExceedsInput(t *testing.T) {
	in := responses2.Usage{InputTokens: 100, OutputTokens: 10}
	in.InputTokensDetails.CachedTokens = 500

	wire := anthropicUsage(&in)
	if wire.InputTokens < 0 {
		t.Fatalf("negative InputTokens: %+v", wire)
	}
	if sum := wire.InputTokens + wire.CacheReadInputTokens; sum != 100 {
		t.Errorf("wire prompt total = %d, want 100 (reported InputTokens preserved)", sum)
	}
}

// TestNativeUsage_FoldsCacheTokensIntoPrompt pins the normalization of
// Anthropic's split accounting: input_tokens counts only the portion of the
// prompt that was neither read from nor written to the cache, so the real
// prompt size is the sum of all three counts. Reporting only input_tokens made
// a nearly-full context look almost empty whenever caching was on.
func TestNativeUsage_FoldsCacheTokensIntoPrompt(t *testing.T) {
	got := nativeUsage(&ChunkMessageUsage{
		InputTokens:              500,
		CacheReadInputTokens:     170000,
		CacheCreationInputTokens: 9500,
		OutputTokens:             300,
	})

	if got.InputTokens != 180000 {
		t.Errorf("InputTokens = %d, want 180000 (500 uncached + 170000 read + 9500 write)", got.InputTokens)
	}
	if got.InputTokensDetails.CachedTokens != 170000 {
		t.Errorf("CachedTokens = %d, want 170000 (cache reads only)", got.InputTokensDetails.CachedTokens)
	}
	if got.OutputTokens != 300 {
		t.Errorf("OutputTokens = %d, want 300", got.OutputTokens)
	}
	if got.TotalTokens != 180300 {
		t.Errorf("TotalTokens = %d, want 180300", got.TotalTokens)
	}
	// The contract's subset rule: cached tokens are part of the prompt, never
	// additional to it.
	if got.InputTokensDetails.CachedTokens > got.InputTokens {
		t.Errorf("CachedTokens (%d) must be a subset of InputTokens (%d)",
			got.InputTokensDetails.CachedTokens, got.InputTokens)
	}
}

func TestNativeUsage_NoCache(t *testing.T) {
	got := nativeUsage(&ChunkMessageUsage{InputTokens: 1200, OutputTokens: 340})

	if got.InputTokens != 1200 || got.OutputTokens != 340 || got.TotalTokens != 1540 {
		t.Fatalf("usage = %+v, want input=1200 output=340 total=1540", got)
	}
	if got.InputTokensDetails.CachedTokens != 0 {
		t.Errorf("CachedTokens = %d, want 0", got.InputTokensDetails.CachedTokens)
	}
}

func TestNativeUsage_Nil(t *testing.T) {
	if got := nativeUsage(nil); got != nil {
		t.Fatalf("nativeUsage(nil) = %+v, want nil", got)
	}
}

// TestStreamUsage_MergesStartAndDelta covers the streaming reconciliation.
// Anthropic reports the prompt counts on message_start and the final
// output_tokens on message_delta; reading message_delta alone dropped the
// entire prompt whenever that event omitted the input counts.
func TestStreamUsage_MergesStartAndDelta(t *testing.T) {
	c := &ResponseChunkToNativeResponseChunkConverter{
		messageStart: &ChunkMessage[ChunkTypeMessageStart]{
			Message: &ChunkMessageData{
				Usage: &ChunkMessageUsage{
					InputTokens:              500,
					CacheReadInputTokens:     170000,
					CacheCreationInputTokens: 9500,
					OutputTokens:             1, // Anthropic's placeholder
				},
			},
		},
		messageDelta: &ChunkMessage[ChunkTypeMessageDelta]{
			Usage: &ChunkMessageUsage{OutputTokens: 300},
		},
	}

	got := c.streamUsage()
	if got == nil {
		t.Fatal("streamUsage() = nil")
	}
	if got.InputTokens != 180000 {
		t.Errorf("InputTokens = %d, want 180000 (from message_start)", got.InputTokens)
	}
	if got.OutputTokens != 300 {
		t.Errorf("OutputTokens = %d, want 300 (from message_delta)", got.OutputTokens)
	}
	if got.TotalTokens != 180300 {
		t.Errorf("TotalTokens = %d, want 180300", got.TotalTokens)
	}
}

// TestStreamUsage_UsageOnlyOnStart covers the shape a thinking turn produces,
// where message_delta carries no usage object at all — the prompt counts must
// still come through from message_start.
func TestStreamUsage_UsageOnlyOnStart(t *testing.T) {
	c := &ResponseChunkToNativeResponseChunkConverter{
		messageStart: &ChunkMessage[ChunkTypeMessageStart]{
			Message: &ChunkMessageData{
				Usage: &ChunkMessageUsage{InputTokens: 2679, OutputTokens: 3},
			},
		},
		messageDelta: &ChunkMessage[ChunkTypeMessageDelta]{}, // no usage
	}

	got := c.streamUsage()
	if got == nil {
		t.Fatal("streamUsage() = nil")
	}
	if got.InputTokens != 2679 || got.OutputTokens != 3 {
		t.Fatalf("usage = %+v, want input=2679 output=3", got)
	}
}

// TestStreamUsage_NoUsageAnywhere covers a turn where neither event carries a
// usage object: the result is nil rather than a panic, and the caller
// substitutes a zero value.
func TestStreamUsage_NoUsageAnywhere(t *testing.T) {
	c := &ResponseChunkToNativeResponseChunkConverter{
		messageStart: &ChunkMessage[ChunkTypeMessageStart]{Message: &ChunkMessageData{}},
		messageDelta: &ChunkMessage[ChunkTypeMessageDelta]{},
	}

	if got := c.streamUsage(); got != nil {
		t.Fatalf("streamUsage() = %+v, want nil", got)
	}
}

// TestStreamUsage_DeltaEchoesInputCounts covers the server-tool-use shape,
// where message_delta repeats the prompt counts. Those counts are cumulative,
// not incremental, so merging must be idempotent rather than doubling them.
func TestStreamUsage_DeltaEchoesInputCounts(t *testing.T) {
	full := &ChunkMessageUsage{
		InputTokens:          500,
		CacheReadInputTokens: 170000,
		OutputTokens:         300,
	}
	c := &ResponseChunkToNativeResponseChunkConverter{
		messageStart: &ChunkMessage[ChunkTypeMessageStart]{
			Message: &ChunkMessageData{Usage: full},
		},
		messageDelta: &ChunkMessage[ChunkTypeMessageDelta]{Usage: full},
	}

	got := c.streamUsage()
	if got.InputTokens != 170500 {
		t.Errorf("InputTokens = %d, want 170500 (not doubled)", got.InputTokens)
	}
	if got.OutputTokens != 300 {
		t.Errorf("OutputTokens = %d, want 300 (not doubled)", got.OutputTokens)
	}
}

// TestBuildResponseCompleted_MissingEvents guards the completion chunk against
// a stream that ends without the events carrying usage — it used to
// dereference both unconditionally.
func TestBuildResponseCompleted_MissingEvents(t *testing.T) {
	c := &ResponseChunkToNativeResponseChunkConverter{}

	chunk := c.buildResponseCompleted()
	if chunk == nil || chunk.OfResponseCompleted == nil {
		t.Fatal("buildResponseCompleted() produced no completion chunk")
	}
	if got := chunk.OfResponseCompleted.Response.Usage.TotalTokens; got != 0 {
		t.Errorf("TotalTokens = %d, want 0", got)
	}
}
