package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"time"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

// CreateSandboxRequest is the request body for creating a sandbox via the API.
type CreateSandboxRequest struct {
	SessionID string            `json:"session_id"`
	Image     string            `json:"image"`
	AgentName string            `json:"agent_name,omitempty"`
	Namespace string            `json:"namespace,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

// Handle represents a running sandbox returned by the API.
type Handle struct {
	SessionID string `json:"session_id"`
	PodName   string `json:"pod_name"`
	PodIP     string `json:"pod_ip"`
	Port      int    `json:"port"`
}

type Manager interface {
	CreateSandbox(ctx context.Context, req *CreateSandboxRequest) (*Handle, error)
	GetSandbox(ctx context.Context, sessionID string) (*Handle, error)
	DeleteSandbox(ctx context.Context, sessionID string) error
}

// BashExecRequest is the request body for daemon exec endpoints (bash/python).
type BashExecRequest struct {
	Command        string            `json:"command,omitempty"`
	Args           []string          `json:"args,omitempty"`
	Script         string            `json:"script,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
	Workdir        string            `json:"workdir,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
}

// BashExecResponse is the response from daemon exec endpoints.
type BashExecResponse struct {
	Stdout        string `json:"stdout"`
	Stderr        string `json:"stderr"`
	ExitCode      int    `json:"exit_code"`
	DurationMilli int64  `json:"duration_ms"`
	Cwd           string `json:"cwd,omitempty"` // current working directory after command execution
}

// FileContent is the response from daemon file read/write.
type FileContent struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

const defaultDaemonPort = 8080

// NewDaemonClient constructs a client for the given sandbox handle (exec and file operations).
func NewDaemonClient(handle *Handle) *DaemonClient {
	port := handle.Port
	if port <= 0 {
		port = defaultDaemonPort
	}
	base := fmt.Sprintf("http://%s:%d", handle.PodIP, port)
	return &DaemonClient{
		baseURL: base,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

// DaemonClient performs exec and file operations against a sandbox daemon.
type DaemonClient struct {
	baseURL    string
	httpClient *http.Client
}

// RunBashCommand executes a bash command inside the sandbox.
func (c *DaemonClient) RunBashCommand(ctx context.Context, in *BashExecRequest) (*BashExecResponse, error) {
	var res BashExecResponse
	if err := c.doJSON(ctx, http.MethodPost, "/exec/bash", in, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// RunPythonScript executes a Python script inside the sandbox.
func (c *DaemonClient) RunPythonScript(ctx context.Context, in *BashExecRequest) (*BashExecResponse, error) {
	var res BashExecResponse
	if err := c.doJSON(ctx, http.MethodPost, "/exec/python", in, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// ReadFile reads a file from the sandbox filesystem.
func (c *DaemonClient) ReadFile(ctx context.Context, filePath string) (*FileContent, error) {
	var out FileContent
	if err := c.doJSON(ctx, http.MethodGet, "/files/"+url.PathEscape(filePath), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// WriteFile writes content to a file in the sandbox filesystem.
func (c *DaemonClient) WriteFile(ctx context.Context, filePath, content string) (*FileContent, error) {
	in := FileContent{Path: filePath, Content: content}
	var out FileContent
	if err := c.doJSON(ctx, http.MethodPost, "/files/"+url.PathEscape(filePath), in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteFile deletes a file from the sandbox filesystem.
func (c *DaemonClient) DeleteFile(ctx context.Context, filePath string) error {
	return c.doJSON(ctx, http.MethodDelete, "/files/"+url.PathEscape(filePath), nil, nil)
}

func (c *DaemonClient) doJSON(ctx context.Context, method, p string, in any, out any) error {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return err
	}
	u.Path = path.Join(u.Path, p)

	var body io.Reader
	if in != nil {
		buf, err := sonic.Marshal(in)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		body = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("sandbox daemon error: status=%d body=%s", resp.StatusCode, string(b))
	}

	if out == nil {
		return nil
	}

	if err := utils.DecodeJSON(resp.Body, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
