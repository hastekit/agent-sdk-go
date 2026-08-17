package hastekitgateway

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

// apiResponse is the JSON shape returned by the agent-server sandbox endpoints.
type apiResponse struct {
	Error   bool            `json:"error"`
	Message string          `json:"message"`
	Data    *sandbox.Handle `json:"data"`
	Status  int             `json:"status"`
}

// SandboxClient calls the agent-server sandbox API via HTTP.
type SandboxClient struct {
	endpoint   string
	httpClient *http.Client
}

// NewSandboxClient creates a client that talks to the agent-server sandbox API.
func (c *Config) NewSandboxClient() *SandboxClient {
	return &SandboxClient{
		endpoint:   strings.TrimSuffix(c.Endpoint, "/"),
		httpClient: c.HttpClient,
	}
}

// CreateSandbox creates a new sandbox and returns its handle.
func (c *SandboxClient) CreateSandbox(ctx context.Context, req *sandbox.CreateSandboxRequest) (*sandbox.Handle, error) {
	if req == nil {
		return nil, fmt.Errorf("CreateSandboxRequest is required")
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.Image = strings.TrimSpace(req.Image)
	if req.SessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	if req.Image == "" {
		return nil, fmt.Errorf("image is required")
	}

	payload, err := sonic.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/api/sandbox", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	var out apiResponse
	if err := utils.DecodeJSON(resp.Body, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if resp.StatusCode >= 400 || out.Error {
		return nil, fmt.Errorf("sandbox API error (status %d): %s", resp.StatusCode, out.Message)
	}

	if out.Data == nil {
		return nil, fmt.Errorf("sandbox API returned no data")
	}
	return out.Data, nil
}

// GetSandbox returns the handle for an existing sandbox by session ID.
func (c *SandboxClient) GetSandbox(ctx context.Context, sessionID string) (*sandbox.Handle, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}

	path := "/api/sandbox/" + url.PathEscape(sessionID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+path, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	var out apiResponse
	if err := utils.DecodeJSON(resp.Body, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if resp.StatusCode >= 400 || out.Error {
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("sandbox not found: %s", sessionID)
		}
		return nil, fmt.Errorf("sandbox API error (status %d): %s", resp.StatusCode, out.Message)
	}

	if out.Data == nil {
		return nil, fmt.Errorf("sandbox API returned no data")
	}
	return out.Data, nil
}

// DeleteSandbox tears down the sandbox for the given session ID.
func (c *SandboxClient) DeleteSandbox(ctx context.Context, sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return fmt.Errorf("session_id is required")
	}

	path := "/api/sandbox/" + url.PathEscape(sessionID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.endpoint+path, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var out apiResponse
		_ = utils.DecodeJSON(resp.Body, &out)
		msg := out.Message
		if msg == "" {
			msg = resp.Status
		}
		return fmt.Errorf("sandbox API error (status %d): %s", resp.StatusCode, msg)
	}
	return nil
}
