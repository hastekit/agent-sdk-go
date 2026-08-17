package hastekitgateway

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/gateway"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/chat_completion"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/embeddings"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/image_edit"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/image_generation"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/speech"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/transcription"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// ExternalLLMGateway calls the agent-server's gateway API via HTTP.
// Use this when you're an SDK consumer calling the agent-server remotely.
type ExternalLLMGateway struct {
	endpoint   string
	httpClient *http.Client
}

type LLMClient struct {
	endpoint   string
	virtualKey string
	httpClient *http.Client
}

// NewLLMClient creates a provider that calls agent-server via HTTP.
func (c *Config) NewLLMClient() *LLMClient {
	return &LLMClient{
		endpoint:   c.Endpoint,
		virtualKey: c.VirtualKey,
		httpClient: c.HttpClient,
	}
}

func (c *LLMClient) Model(id string) llm.Provider {
	i := strings.SplitN(id, "/", 2)

	return gateway.NewLLMClient(
		&ExternalLLMGateway{
			endpoint:   strings.TrimSuffix(c.endpoint, "/"),
			httpClient: c.httpClient,
		},
		nil,
		gateway.WithKey(c.virtualKey),
		gateway.WithModel(llm.ProviderName(i[0]), i[1]),
	)
}

func (p *ExternalLLMGateway) NewResponses(ctx context.Context, providerName llm.ProviderName, key string, req *responses.Request) (*responses.Response, error) {
	// Prepend provider to model for gateway routing
	originalModel := req.Model
	req.Model = fmt.Sprintf("%s:%s", providerName, req.Model)
	defer func() { req.Model = originalModel }()

	payload, err := sonic.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint+"/api/gateway/responses", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	for k, v := range carrier {
		httpReq.Header.Add(k, v)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-virtual-key", key)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var errResp map[string]any
		_ = utils.DecodeJSON(resp.Body, &errResp)
		return nil, fmt.Errorf("gateway error (status %d): %v", resp.StatusCode, errResp)
	}

	var nativeResp responses.Response
	if err := utils.DecodeJSON(resp.Body, &nativeResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &nativeResp, nil
}

func (p *ExternalLLMGateway) NewStreamingResponses(ctx context.Context, providerName llm.ProviderName, key string, req *responses.Request) (chan *responses.ResponseChunk, error) {
	// Prepend provider to model for gateway routing
	originalModel := req.Model
	req.Model = fmt.Sprintf("%s:%s", providerName, req.Model)

	stream := true
	req.Stream = &stream

	payload, err := sonic.Marshal(req)
	if err != nil {
		req.Model = originalModel
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}
	req.Model = originalModel

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint+"/api/gateway/responses", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	for k, v := range carrier {
		httpReq.Header.Add(k, v)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-virtual-key", key)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		var errResp map[string]any
		_ = utils.DecodeJSON(resp.Body, &errResp)
		return nil, fmt.Errorf("gateway error (status %d): %v", resp.StatusCode, errResp)
	}

	out := make(chan *responses.ResponseChunk)
	go func() {
		defer resp.Body.Close()
		defer close(out)

		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}

			line = strings.TrimRight(line, "\r\n")
			if !strings.HasPrefix(line, "data:") {
				continue
			}

			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimSpace(data)
			if data == "" || data == "[DONE]" {
				continue
			}

			var chunk responses.ResponseChunk
			if err := sonic.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}

			out <- &chunk
		}
	}()

	return out, nil
}

func (p *ExternalLLMGateway) NewEmbedding(ctx context.Context, providerName llm.ProviderName, key string, req *embeddings.Request) (*embeddings.Response, error) {
	// Prepend provider to model for gateway routing
	originalModel := req.Model
	req.Model = fmt.Sprintf("%s:%s", providerName, req.Model)
	defer func() { req.Model = originalModel }()

	payload, err := sonic.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint+"/api/gateway/embeddings", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	for k, v := range carrier {
		httpReq.Header.Add(k, v)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-virtual-key", key)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var errResp map[string]any
		_ = utils.DecodeJSON(resp.Body, &errResp)
		return nil, fmt.Errorf("gateway error (status %d): %v", resp.StatusCode, errResp)
	}

	var nativeResp embeddings.Response
	if err := utils.DecodeJSON(resp.Body, &nativeResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &nativeResp, nil
}

func (p *ExternalLLMGateway) NewChatCompletion(ctx context.Context, providerName llm.ProviderName, key string, req *chat_completion.Request) (*chat_completion.Response, error) {
	// Prepend provider to model for gateway routing
	originalModel := req.Model
	req.Model = fmt.Sprintf("%s:%s", providerName, req.Model)
	defer func() { req.Model = originalModel }()

	payload, err := sonic.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint+"/api/gateway/chat_completions", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	for k, v := range carrier {
		httpReq.Header.Add(k, v)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-virtual-key", key)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var errResp map[string]any
		_ = utils.DecodeJSON(resp.Body, &errResp)
		return nil, fmt.Errorf("gateway error (status %d): %v", resp.StatusCode, errResp)
	}

	var nativeResp chat_completion.Response
	if err := utils.DecodeJSON(resp.Body, &nativeResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &nativeResp, nil
}

func (p *ExternalLLMGateway) NewStreamingChatCompletion(ctx context.Context, providerName llm.ProviderName, key string, req *chat_completion.Request) (chan *chat_completion.ResponseChunk, error) {
	// Prepend provider to model for gateway routing
	originalModel := req.Model
	req.Model = fmt.Sprintf("%s:%s", providerName, req.Model)

	stream := true
	req.Stream = &stream

	payload, err := sonic.Marshal(req)
	if err != nil {
		req.Model = originalModel
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}
	req.Model = originalModel

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint+"/api/gateway/chat_completions", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	for k, v := range carrier {
		httpReq.Header.Add(k, v)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-virtual-key", key)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		var errResp map[string]any
		_ = utils.DecodeJSON(resp.Body, &errResp)
		return nil, fmt.Errorf("gateway error (status %d): %v", resp.StatusCode, errResp)
	}

	out := make(chan *chat_completion.ResponseChunk)
	go func() {
		defer resp.Body.Close()
		defer close(out)

		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}

			line = strings.TrimRight(line, "\r\n")
			if !strings.HasPrefix(line, "data:") {
				continue
			}

			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimSpace(data)
			if data == "" || data == "[DONE]" {
				continue
			}

			var chunk chat_completion.ResponseChunk
			if err := sonic.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}

			out <- &chunk
		}
	}()

	return out, nil
}

func (p *ExternalLLMGateway) NewSpeech(ctx context.Context, providerName llm.ProviderName, key string, req *speech.Request) (*speech.Response, error) {
	// Prepend provider to model for gateway routing
	originalModel := req.Model
	req.Model = fmt.Sprintf("%s:%s", providerName, req.Model)
	defer func() { req.Model = originalModel }()

	payload, err := sonic.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint+"/api/gateway/speech", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	for k, v := range carrier {
		httpReq.Header.Add(k, v)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-virtual-key", key)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var errResp map[string]any
		_ = utils.DecodeJSON(resp.Body, &errResp)
		return nil, fmt.Errorf("gateway error (status %d): %v", resp.StatusCode, errResp)
	}

	var nativeResp speech.Response
	if err := utils.DecodeJSON(resp.Body, &nativeResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &nativeResp, nil
}

func (p *ExternalLLMGateway) NewStreamingSpeech(ctx context.Context, providerName llm.ProviderName, key string, req *speech.Request) (chan *speech.ResponseChunk, error) {
	// Prepend provider to model for gateway routing
	originalModel := req.Model
	req.Model = fmt.Sprintf("%s:%s", providerName, req.Model)

	payload, err := sonic.Marshal(req)
	if err != nil {
		req.Model = originalModel
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}
	req.Model = originalModel

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint+"/api/gateway/speech", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	for k, v := range carrier {
		httpReq.Header.Add(k, v)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-virtual-key", key)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		var errResp map[string]any
		_ = utils.DecodeJSON(resp.Body, &errResp)
		return nil, fmt.Errorf("gateway error (status %d): %v", resp.StatusCode, errResp)
	}

	out := make(chan *speech.ResponseChunk)
	go func() {
		defer resp.Body.Close()
		defer close(out)

		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}

			line = strings.TrimRight(line, "\r\n")
			if !strings.HasPrefix(line, "data:") {
				continue
			}

			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimSpace(data)
			if data == "" || data == "[DONE]" {
				continue
			}

			var chunk speech.ResponseChunk
			if err := sonic.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}

			out <- &chunk
		}
	}()

	return out, nil
}

func (p *ExternalLLMGateway) NewTranscription(ctx context.Context, providerName llm.ProviderName, key string, req *transcription.Request) (*transcription.Response, error) {
	// Prepend provider to model for gateway routing
	originalModel := req.Model
	req.Model = fmt.Sprintf("%s:%s", providerName, req.Model)
	defer func() { req.Model = originalModel }()

	payload, err := sonic.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint+"/api/gateway/transcriptions", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	for k, v := range carrier {
		httpReq.Header.Add(k, v)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-virtual-key", key)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var errResp map[string]any
		_ = utils.DecodeJSON(resp.Body, &errResp)
		return nil, fmt.Errorf("gateway error (status %d): %v", resp.StatusCode, errResp)
	}

	var nativeResp transcription.Response
	if err := utils.DecodeJSON(resp.Body, &nativeResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &nativeResp, nil
}

func (p *ExternalLLMGateway) NewImageGeneration(ctx context.Context, providerName llm.ProviderName, key string, req *image_generation.Request) (*image_generation.Response, error) {
	// Prepend provider to model for gateway routing
	originalModel := req.Model
	req.Model = fmt.Sprintf("%s:%s", providerName, req.Model)
	defer func() { req.Model = originalModel }()

	payload, err := sonic.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint+"/api/gateway/images/generations", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	for k, v := range carrier {
		httpReq.Header.Add(k, v)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-virtual-key", key)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var errResp map[string]any
		_ = utils.DecodeJSON(resp.Body, &errResp)
		return nil, fmt.Errorf("gateway error (status %d): %v", resp.StatusCode, errResp)
	}

	var nativeImageResp image_generation.Response
	if err := utils.DecodeJSON(resp.Body, &nativeImageResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &nativeImageResp, nil
}

func (p *ExternalLLMGateway) NewImageEdit(ctx context.Context, providerName llm.ProviderName, key string, req *image_edit.Request) (*image_edit.Response, error) {
	// Prepend provider to model for gateway routing
	originalModel := req.Model
	req.Model = fmt.Sprintf("%s:%s", providerName, req.Model)
	defer func() { req.Model = originalModel }()

	payload, err := sonic.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint+"/api/gateway/images/edits", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	for k, v := range carrier {
		httpReq.Header.Add(k, v)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-virtual-key", key)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var errResp map[string]any
		_ = utils.DecodeJSON(resp.Body, &errResp)
		return nil, fmt.Errorf("gateway error (status %d): %v", resp.StatusCode, errResp)
	}

	var nativeResp image_edit.Response
	if err := utils.DecodeJSON(resp.Body, &nativeResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &nativeResp, nil
}
