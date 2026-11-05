package hastekitgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
)

// SandboxClient implements sandbox.Provider using HasteKit Gateway's sandbox
// routes. Custom in-process backends implement sandbox.Provider directly.
type SandboxClient struct {
	baseURL string
	client  *http.Client
}

// NewSandboxClient routes lifecycle and execution through the gateway's configured
// provider. It never connects directly to a pod address.
func (c *Config) NewSandboxClient() *SandboxClient {
	return &SandboxClient{baseURL: strings.TrimRight(c.Endpoint, "/") + "/api/sandbox", client: c.HttpClient}
}

func (p *SandboxClient) runtime(ctx context.Context, ref sandbox.Reference, method string, input any) (sandbox.Sandbox, error) {
	endpoint := p.baseURL
	if method == http.MethodPost {
		endpoint += "/"
	} else {
		if ref.ID == "" || ref.Provider == "" {
			return nil, fmt.Errorf("sandbox reference is required")
		}
		endpoint += "/" + url.PathEscape(ref.Provider) + "/" + url.PathEscape(ref.ID)
	}
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	client := p.client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		var cause error
		switch response.StatusCode {
		case http.StatusNotFound:
			cause = sandbox.ErrNotFound
		case http.StatusConflict:
			cause = sandbox.ErrConflict
		case http.StatusNotImplemented:
			cause = sandbox.ErrUnsupported
		case http.StatusGatewayTimeout:
			cause = context.DeadlineExceeded
		}
		if cause != nil {
			return nil, fmt.Errorf("gateway sandbox HTTP %d: %w: %s", response.StatusCode, cause, data)
		}
		return nil, fmt.Errorf("gateway sandbox HTTP %d: %s", response.StatusCode, data)
	}
	if method == http.MethodDelete {
		return nil, nil
	}
	var result sandbox.Reference
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return nil, err
	}
	if result.ID == "" || result.Provider == "" {
		return nil, fmt.Errorf("gateway returned an invalid sandbox reference")
	}
	if method != http.MethodPost && result != ref {
		return nil, sandbox.ErrConflict
	}
	// Do not expose EndpointResolver: gateway routes do not provide arbitrary
	// service ports or direct access to the backend's address.
	return &gatewaySandbox{ref: result, endpoint: p.baseURL + "/" + url.PathEscape(result.Provider) + "/" + url.PathEscape(result.ID), client: client}, nil
}
func (p *SandboxClient) Create(ctx context.Context, req sandbox.CreateRequest) (sandbox.Sandbox, error) {
	return p.runtime(ctx, sandbox.Reference{}, http.MethodPost, req)
}
func (p *SandboxClient) Connect(ctx context.Context, ref sandbox.Reference) (sandbox.Sandbox, error) {
	return p.runtime(ctx, ref, http.MethodGet, nil)
}
func (p *SandboxClient) Delete(ctx context.Context, ref sandbox.Reference) error {
	_, err := p.runtime(ctx, ref, http.MethodDelete, nil)
	return err
}

// gatewaySandbox runs commands through the gateway's routes.
type gatewaySandbox struct {
	ref      sandbox.Reference
	endpoint string
	client   *http.Client
}

func (s *gatewaySandbox) Reference() sandbox.Reference { return s.ref }

func (s *gatewaySandbox) request(ctx context.Context, method, route string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.endpoint+route, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("X-Sandbox-Provider", s.ref.Provider)
	req.Header.Set("X-Sandbox-ID", s.ref.ID)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		switch resp.StatusCode {
		case http.StatusNotFound:
			return nil, fmt.Errorf("%w: %s", sandbox.ErrNotFound, b)
		case http.StatusConflict:
			return nil, fmt.Errorf("%w: %s", sandbox.ErrConflict, b)
		case http.StatusNotImplemented:
			return nil, fmt.Errorf("%w: %s", sandbox.ErrUnsupported, b)
		}
		return nil, fmt.Errorf("gateway sandbox HTTP %d: %s", resp.StatusCode, b)
	}
	return resp, nil
}

func (s *gatewaySandbox) Exec(ctx context.Context, req sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	timeout, err := sandbox.ValidateExec(req)
	if err != nil {
		return nil, err
	}
	req.Timeout = timeout
	ctx, cancel := context.WithTimeout(ctx, timeout+10*time.Second)
	defer cancel()
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	resp, err := s.request(ctx, http.MethodPost, "/v2/exec", bytes.NewReader(b), "application/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result sandbox.ExecResult
	err = json.NewDecoder(resp.Body).Decode(&result)
	return &result, err
}

var _ sandbox.Provider = (*SandboxClient)(nil)
