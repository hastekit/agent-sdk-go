// Package daytona implements sandbox.Provider using Daytona's control and toolbox APIs.
package daytona

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox/internal/httpapi"
)

type Profile struct {
	Snapshot string
	User     string
	Target   string
	TTL      time.Duration
}
type Config struct {
	APIKey       string
	BaseURL      string
	HTTPClient   *http.Client
	Profiles     map[string]Profile
	ReadyTimeout time.Duration
}
type Provider struct {
	api    *httpapi.Client
	config Config
}
type info struct {
	ID              string            `json:"id"`
	State           string            `json:"state"`
	ToolboxProxyURL string            `json:"toolboxProxyUrl"`
	ErrorReason     string            `json:"errorReason"`
	Labels          map[string]string `json:"labels"`
	Snapshot        string            `json:"snapshot"`
}

func New(cfg Config) (*Provider, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("daytona API key is required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://app.daytona.io/api"
	}
	if cfg.ReadyTimeout == 0 {
		cfg.ReadyTimeout = 2 * time.Minute
	}
	if cfg.ReadyTimeout < 0 {
		return nil, fmt.Errorf("negative readiness timeout")
	}
	profiles := make(map[string]Profile, len(cfg.Profiles))
	for k, v := range cfg.Profiles {
		profiles[k] = v
	}
	cfg.Profiles = profiles
	api, err := httpapi.New(cfg.BaseURL, cfg.HTTPClient, http.Header{"Authorization": {"Bearer " + cfg.APIKey}})
	if err != nil {
		return nil, err
	}
	return &Provider{api, cfg}, nil
}

func (p *Provider) Create(ctx context.Context, req sandbox.CreateRequest) (sandbox.Sandbox, error) {
	profile, ok := p.config.Profiles[req.Profile]
	if !ok {
		return nil, fmt.Errorf("unknown daytona profile %q", req.Profile)
	}
	ttl := req.TTL
	if ttl == 0 {
		ttl = profile.TTL
	}
	if ttl < 0 {
		return nil, fmt.Errorf("negative TTL")
	}
	body := map[string]any{"snapshot": profile.Snapshot, "env": req.Env, "public": false, "labels": map[string]string{"hastekit.namespace": req.Session.Namespace, "hastekit.session": req.Session.SessionID, "hastekit.agent": req.AgentName}}
	name := ""
	if req.Session.Namespace != "" && req.Session.SessionID != "" {
		name = "hastekit-" + sandbox.ResourceID(req.Session)
		body["name"] = name
		body["labels"].(map[string]string)["hastekit.request"] = sandbox.RequestFingerprint(req)
	}
	if profile.User != "" {
		body["user"] = profile.User
	}
	if profile.Target != "" {
		body["target"] = profile.Target
	}
	if ttl > 0 {
		body["ttlMinutes"] = int64((ttl + time.Minute - 1) / time.Minute)
	}
	ctx, cancel := context.WithTimeout(ctx, p.config.ReadyTimeout)
	defer cancel()
	var i info
	if err := p.api.JSON(ctx, "POST", "/sandbox", body, &i); err != nil {
		if !errors.Is(err, sandbox.ErrConflict) || name == "" {
			return nil, err
		}
		// Never delete a resource owned by the winning creator. Recover only a
		// matching named sandbox, not arbitrary 409 responses.
		var existing info
		if lookupErr := p.api.JSON(ctx, "GET", "/sandbox/"+url.PathEscape(name), nil, &existing); lookupErr != nil {
			return nil, errors.Join(err, lookupErr)
		}
		if existing.Labels["hastekit.request"] != sandbox.RequestFingerprint(req) || existing.Snapshot != profile.Snapshot {
			return nil, fmt.Errorf("%w: named sandbox has different configuration", sandbox.ErrConflict)
		}
		return p.connect(ctx, existing)
	}
	if i.ID == "" {
		return nil, fmt.Errorf("daytona create returned no ID")
	}
	s, err := p.ready(ctx, i)
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		err = errors.Join(err, p.Delete(cleanup, sandbox.Reference{Provider: "daytona", ID: i.ID}))
	}
	return s, err
}

func (p *Provider) Connect(ctx context.Context, ref sandbox.Reference) (sandbox.Sandbox, error) {
	if err := validate(ref); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.config.ReadyTimeout)
	defer cancel()
	var i info
	if err := p.api.JSON(ctx, "GET", "/sandbox/"+url.PathEscape(ref.ID), nil, &i); err != nil {
		return nil, err
	}
	return p.connect(ctx, i)
}

func (p *Provider) connect(ctx context.Context, i info) (sandbox.Sandbox, error) {
	if i.State == "stopped" || i.State == "archived" {
		if err := p.api.JSON(ctx, "POST", "/sandbox/"+url.PathEscape(i.ID)+"/start", nil, nil); err != nil {
			if !errors.Is(err, sandbox.ErrConflict) {
				return nil, err
			}
			var current info
			if lookupErr := p.api.JSON(ctx, "GET", "/sandbox/"+url.PathEscape(i.ID), nil, &current); lookupErr != nil {
				return nil, errors.Join(err, lookupErr)
			}
			if current.ID != i.ID || (current.State != "started" && current.State != "starting") {
				return nil, err
			}
			return p.ready(ctx, current)
		}
		i.State = "starting"
	}

	return p.ready(ctx, i)
}

func (p *Provider) ready(ctx context.Context, i info) (sandbox.Sandbox, error) {
	for {
		if i.ID == "" {
			return nil, fmt.Errorf("daytona returned no ID")
		}
		switch i.State {
		case "started":
			api, err := httpapi.New(i.ToolboxProxyURL+"/"+url.PathEscape(i.ID), p.config.HTTPClient, p.api.Headers)
			if err != nil {
				return nil, err
			}
			return &runtime{ref: sandbox.Reference{Provider: "daytona", ID: i.ID}, api: api}, nil
		case "error", "build_failed", "destroyed", "destroying":
			return nil, fmt.Errorf("daytona sandbox %s: %s %s", i.ID, i.State, i.ErrorReason)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
		if err := p.api.JSON(ctx, "GET", "/sandbox/"+url.PathEscape(i.ID), nil, &i); err != nil {
			return nil, err
		}
	}
}

func validate(ref sandbox.Reference) error {
	if ref.Provider != "daytona" || ref.ID == "" {
		return fmt.Errorf("invalid daytona reference")
	}
	return nil
}

func (p *Provider) Delete(ctx context.Context, ref sandbox.Reference) error {
	if err := validate(ref); err != nil {
		return err
	}
	err := p.api.JSON(ctx, "DELETE", "/sandbox/"+url.PathEscape(ref.ID), nil, nil)
	if errors.Is(err, sandbox.ErrNotFound) {
		return nil
	}
	return err
}

type runtime struct {
	ref sandbox.Reference
	api *httpapi.Client
}

func (s *runtime) Reference() sandbox.Reference { return s.ref }
func (s *runtime) Files() sandbox.FileSystem    { return s }
func (s *runtime) Exec(ctx context.Context, req sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	timeout, err := sandbox.ValidateExec(req)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout+10*time.Second)
	defer cancel()
	body := map[string]any{"command": sandbox.QuoteArgv(req.Argv), "cwd": req.Workdir, "envs": req.Env, "timeout": int64((timeout + time.Second - 1) / time.Second)}
	var result struct {
		Result   string `json:"result"`
		ExitCode int    `json:"exitCode"`
	}
	start := time.Now()
	if err := s.api.JSON(ctx, "POST", "/process/execute", body, &result); err != nil {
		return nil, err
	}
	return &sandbox.ExecResult{Stdout: result.Result, ExitCode: result.ExitCode, OutputCombined: true, DurationMilli: time.Since(start).Milliseconds()}, nil
}

func (s *runtime) Read(ctx context.Context, path string) (io.ReadCloser, error) {
	r, err := s.api.Do(ctx, "GET", "/files/download?path="+url.QueryEscape(path), nil, "")
	if err != nil {
		return nil, err
	}
	return r.Body, nil
}

func (s *runtime) Write(ctx context.Context, path string, content io.Reader) error {
	r, err := s.api.Do(ctx, "POST", "/files/upload-v2?path="+url.QueryEscape(path), content, "application/octet-stream")
	if err != nil {
		return err
	}
	return r.Body.Close()
}

func (s *runtime) Remove(ctx context.Context, path string) error {
	return s.api.JSON(ctx, "DELETE", "/files?path="+url.QueryEscape(path), nil, nil)
}

var _ sandbox.Provider = (*Provider)(nil)
