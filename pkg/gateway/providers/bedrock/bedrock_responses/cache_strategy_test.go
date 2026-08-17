package bedrock_responses

import (
	"strings"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

func TestCacheStrategyMetaFieldIsStripped(t *testing.T) {
	in := &responses.Request{
		Model: "anthropic.claude-test",
		Input: responses.InputUnion{OfString: utils.Ptr("hi")},
		Parameters: responses.Parameters{
			ExtraFields: map[string]any{
				"cache_strategy": "default",
			},
		},
	}

	out := NativeRequestToConverseRequest(in)
	payload, err := sonic.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}

	body := string(payload)
	t.Logf("payload: %s", body)

	if strings.Contains(body, "cache_strategy") {
		t.Fatalf("payload contains cache_strategy: %s", body)
	}
	if !strings.Contains(body, `"cachePoint":{"type":"default"}`) {
		t.Fatalf("payload missing cachePoint with type=default: %s", body)
	}
	if strings.Contains(body, `"ttl"`) {
		t.Fatalf("payload should not contain ttl when cache_ttl is unset: %s", body)
	}
}

func TestCacheStrategyAlwaysEmitsDefaultType(t *testing.T) {
	// Even if the caller passes a non-"default" strategy value, we must still
	// emit type="default" — Bedrock rejects any other value.
	in := &responses.Request{
		Model: "anthropic.claude-test",
		Input: responses.InputUnion{OfString: utils.Ptr("hi")},
		Parameters: responses.Parameters{
			ExtraFields: map[string]any{
				"cache_strategy": "auto",
			},
		},
	}

	out := NativeRequestToConverseRequest(in)
	payload, _ := sonic.Marshal(out)
	body := string(payload)

	if !strings.Contains(body, `"cachePoint":{"type":"default"}`) {
		t.Fatalf("expected cachePoint type=default regardless of strategy value: %s", body)
	}
	if strings.Contains(body, `"type":"auto"`) {
		t.Fatalf("strategy value leaked into cachePoint type: %s", body)
	}
}

func TestCacheTTLPropagates(t *testing.T) {
	in := &responses.Request{
		Model: "anthropic.claude-test",
		Input: responses.InputUnion{OfString: utils.Ptr("hi")},
		Parameters: responses.Parameters{
			ExtraFields: map[string]any{
				"cache_strategy": "default",
				"cache_ttl":      "1h",
			},
		},
	}

	out := NativeRequestToConverseRequest(in)
	payload, _ := sonic.Marshal(out)
	body := string(payload)
	t.Logf("payload: %s", body)

	if !strings.Contains(body, `"cachePoint":{"type":"default","ttl":"1h"}`) {
		t.Fatalf("expected cachePoint with ttl=1h: %s", body)
	}
	if strings.Contains(body, "cache_ttl") {
		t.Fatalf("cache_ttl meta-field leaked into payload: %s", body)
	}
}
