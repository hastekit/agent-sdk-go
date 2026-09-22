package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DaemonConfig connects to the HasteKit daemon's v2 protocol. Transport can supply
// authentication or TLS. The daemon address is never part of Reference.
type DaemonConfig struct {
	Reference Reference
	Endpoint  string
	Client    *http.Client
}
type DaemonSandbox struct {
	ref      Reference
	endpoint string
	client   *http.Client
}

func NewDaemonSandbox(cfg DaemonConfig) (*DaemonSandbox, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("sandbox: invalid daemon endpoint")
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{}
	}
	return &DaemonSandbox{ref: cfg.Reference, endpoint: strings.TrimRight(cfg.Endpoint, "/"), client: cfg.Client}, nil
}
func (s *DaemonSandbox) Reference() Reference { return s.ref }
func (s *DaemonSandbox) Files() FileSystem    { return s }

// KeepAlive refreshes this daemon's idle timer while an external service is busy.
func (s *DaemonSandbox) KeepAlive(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	r, err := s.request(ctx, "GET", "/health", nil, "")
	if err != nil {
		return err
	}
	return r.Body.Close()
}
func (s *DaemonSandbox) Endpoint(_ context.Context, port int) (string, error) {
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("invalid port")
	}
	u, _ := url.Parse(s.endpoint)
	u.Host = net.JoinHostPort(u.Hostname(), strconv.Itoa(port))
	return u.String(), nil
}

// WaitReady verifies daemon readiness even when provisioning reused a runtime.
func (s *DaemonSandbox) WaitReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		probe, done := context.WithTimeout(ctx, 2*time.Second)
		r, err := s.request(probe, "GET", "/health", nil, "")
		if err == nil {
			r.Body.Close()
		}
		done()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("sandbox readiness: %w", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}
func (s *DaemonSandbox) request(ctx context.Context, method, route string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.endpoint+route, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if s.ref.ID != "" {
		req.Header.Set("X-Sandbox-Provider", s.ref.Provider)
		req.Header.Set("X-Sandbox-ID", s.ref.ID)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if resp.StatusCode == 404 {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, b)
		}
		if resp.StatusCode == 409 {
			return nil, fmt.Errorf("%w: %s", ErrConflict, b)
		}
		if resp.StatusCode == 501 {
			return nil, fmt.Errorf("%w: %s", ErrUnsupported, b)
		}
		return nil, fmt.Errorf("sandbox daemon: HTTP %d: %s", resp.StatusCode, b)
	}
	return resp, nil
}
func (s *DaemonSandbox) Exec(ctx context.Context, req ExecRequest) (*ExecResult, error) {
	timeout, err := ValidateExec(req)
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
	resp, err := s.request(ctx, "POST", "/v2/exec", bytes.NewReader(b), "application/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result ExecResult
	err = json.NewDecoder(resp.Body).Decode(&result)
	return &result, err
}
func (s *DaemonSandbox) Read(ctx context.Context, name string) (io.ReadCloser, error) {
	resp, err := s.request(ctx, "GET", "/v2/files?path="+url.QueryEscape(name), nil, "")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}
func (s *DaemonSandbox) Write(ctx context.Context, name string, content io.Reader) error {
	resp, err := s.request(ctx, "PUT", "/v2/files?path="+url.QueryEscape(name), content, "application/octet-stream")
	if err != nil {
		return err
	}
	return resp.Body.Close()
}
func (s *DaemonSandbox) Remove(ctx context.Context, name string) error {
	resp, err := s.request(ctx, "DELETE", "/v2/files?path="+url.QueryEscape(name), nil, "")
	if err != nil {
		return err
	}
	return resp.Body.Close()
}
