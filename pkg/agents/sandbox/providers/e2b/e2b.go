// Package e2b uses the E2B lifecycle API and envd's native Connect/file protocols.
package e2b

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox/internal/httpapi"
)

type Profile struct {
	TemplateID string
	TTL        time.Duration
}
type Config struct {
	APIKey     string
	BaseURL    string
	HTTPClient *http.Client
	Profiles   map[string]Profile
	User       string
	// EnvdURL overrides routing for self-hosted installations. No credentials are passed.
	EnvdURL func(sandboxID, domain string) string
}
type Provider struct {
	api    *httpapi.Client
	config Config
}
type info struct {
	ID     string `json:"sandboxID"`
	Domain string `json:"domain"`
	Token  string `json:"envdAccessToken"`
	State  string `json:"state"`
}

func New(cfg Config) (*Provider, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("e2b API key is required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.e2b.dev"
	}
	if cfg.User == "" {
		cfg.User = "user"
	}
	profiles := make(map[string]Profile, len(cfg.Profiles))
	for k, v := range cfg.Profiles {
		profiles[k] = v
	}
	cfg.Profiles = profiles
	api, err := httpapi.New(cfg.BaseURL, cfg.HTTPClient, http.Header{"X-Api-Key": {cfg.APIKey}})
	if err != nil {
		return nil, err
	}
	return &Provider{api, cfg}, nil
}
func (p *Provider) Create(ctx context.Context, req sandbox.CreateRequest) (sandbox.Sandbox, error) {
	profile, ok := p.config.Profiles[req.Profile]
	if !ok || profile.TemplateID == "" {
		return nil, fmt.Errorf("unknown or empty e2b profile %q", req.Profile)
	}
	ttl := req.TTL
	if ttl == 0 {
		ttl = profile.TTL
	}
	if ttl == 0 {
		ttl = 5 * time.Minute
	}
	if ttl < 0 {
		return nil, fmt.Errorf("negative TTL")
	}
	body := map[string]any{"templateID": profile.TemplateID, "timeout": int64((ttl + time.Second - 1) / time.Second), "secure": true, "envVars": req.Env, "metadata": map[string]string{"namespace": req.Session.Namespace, "session": req.Session.SessionID, "agent": req.AgentName}}
	var i info
	if err := p.api.JSON(ctx, "POST", "/sandboxes", body, &i); err != nil {
		return nil, err
	}
	s, err := p.wrap(i)
	if err != nil && i.ID != "" {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		err = errors.Join(err, p.Delete(cleanup, sandbox.Reference{Provider: "e2b", ID: i.ID}))
	}
	return s, err
}
func validate(ref sandbox.Reference) error {
	if ref.Provider != "e2b" || ref.ID == "" {
		return fmt.Errorf("invalid e2b reference")
	}
	return nil
}
func (p *Provider) Connect(ctx context.Context, ref sandbox.Reference) (sandbox.Sandbox, error) {
	if err := validate(ref); err != nil {
		return nil, err
	}
	var i info
	// GET retains the sandbox's existing lifetime; connecting must not renew its TTL.
	if err := p.api.JSON(ctx, "GET", "/sandboxes/"+url.PathEscape(ref.ID), nil, &i); err != nil {
		return nil, err
	}
	return p.wrap(i)
}
func (p *Provider) Delete(ctx context.Context, ref sandbox.Reference) error {
	if err := validate(ref); err != nil {
		return err
	}
	err := p.api.JSON(ctx, "DELETE", "/sandboxes/"+url.PathEscape(ref.ID), nil, nil)
	if errors.Is(err, sandbox.ErrNotFound) {
		return nil
	}
	return err
}
func (p *Provider) wrap(i info) (sandbox.Sandbox, error) {
	if i.ID == "" {
		return nil, fmt.Errorf("e2b returned no sandbox ID")
	}
	if i.State != "" && i.State != "running" {
		return nil, fmt.Errorf("%w: e2b sandbox state %s requires explicit lifecycle management", sandbox.ErrUnsupported, i.State)
	}
	if i.Domain == "" {
		i.Domain = "e2b.app"
	}
	endpoint := "https://49983-" + i.ID + "." + i.Domain
	if p.config.EnvdURL != nil {
		endpoint = p.config.EnvdURL(i.ID, i.Domain)
	}
	headers := http.Header{"X-Access-Token": {i.Token}, "Authorization": {"Basic " + base64.StdEncoding.EncodeToString([]byte(p.config.User+":"))}, "Connect-Protocol-Version": {"1"}}
	headers.Set("E2b-Sandbox-Id", i.ID)
	headers.Set("E2b-Sandbox-Port", "49983")
	api, err := httpapi.New(endpoint, p.config.HTTPClient, headers)
	if err != nil {
		return nil, err
	}
	return &runtime{ref: sandbox.Reference{Provider: "e2b", ID: i.ID}, api: api, user: p.config.User}, nil
}

type runtime struct {
	ref  sandbox.Reference
	api  *httpapi.Client
	user string
}

func (s *runtime) Reference() sandbox.Reference { return s.ref }
func (s *runtime) Files() sandbox.FileSystem    { return s }
func frame(data []byte) []byte {
	b := make([]byte, 5+len(data))
	binary.BigEndian.PutUint32(b[1:5], uint32(len(data)))
	copy(b[5:], data)
	return b
}
func (s *runtime) Exec(ctx context.Context, req sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	timeout, err := sandbox.ValidateExec(req)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	process := map[string]any{"cmd": req.Argv[0], "args": req.Argv[1:], "envs": req.Env}
	if req.Workdir != "" {
		process["cwd"] = req.Workdir
	}
	body, _ := json.Marshal(map[string]any{"process": process, "stdin": false})
	start := time.Now()
	r, err := s.api.Do(ctx, "POST", "/process.Process/Start", bytes.NewReader(frame(body)), "application/connect+json")
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	var pid int
	ended := false
	defer func() {
		if pid != 0 && !ended {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = s.api.JSON(cleanup, "POST", "/process.Process/SendSignal", map[string]any{"process": map[string]int{"pid": pid}, "signal": "SIGNAL_SIGKILL"}, nil)
		}
	}()
	result := &sandbox.ExecResult{}
	var stdout, stderr bytes.Buffer
	appendOutput := func(b *bytes.Buffer, data []byte) {
		left := (4 << 20) - b.Len()
		if len(data) > left {
			data = data[:left]
			result.OutputTruncated = true
		}
		b.Write(data)
	}
	for {
		var header [5]byte
		if _, err := io.ReadFull(r.Body, header[:]); err != nil {
			return nil, fmt.Errorf("e2b process stream interrupted: %w", err)
		}
		size := binary.BigEndian.Uint32(header[1:])
		if size > 16<<20 {
			return nil, fmt.Errorf("e2b process frame too large")
		}
		data := make([]byte, int(size))
		if _, err := io.ReadFull(r.Body, data); err != nil {
			return nil, err
		}
		if header[0] == 2 {
			var trailer struct {
				Error *struct{ Code, Message string } `json:"error"`
			}
			if err := json.Unmarshal(data, &trailer); err != nil {
				return nil, err
			}
			if trailer.Error != nil {
				return nil, fmt.Errorf("e2b process %s: %s", trailer.Error.Code, trailer.Error.Message)
			}
			if !ended {
				return nil, fmt.Errorf("e2b process stream ended without an exit event")
			}
			result.Stdout = stdout.String()
			result.Stderr = stderr.String()
			result.DurationMilli = time.Since(start).Milliseconds()
			return result, nil
		}
		if header[0] != 0 {
			return nil, fmt.Errorf("unsupported e2b frame flags %d", header[0])
		}
		var message struct {
			Event struct {
				Start *struct {
					PID int `json:"pid"`
				} `json:"start"`
				Data *struct{ Stdout, Stderr []byte } `json:"data"`
				End  *struct {
					ExitCode int    `json:"exitCode"`
					Exited   bool   `json:"exited"`
					Error    string `json:"error"`
				} `json:"end"`
			} `json:"event"`
		}
		if err := json.Unmarshal(data, &message); err != nil {
			return nil, err
		}
		if e := message.Event.Start; e != nil {
			pid = e.PID
		}
		if e := message.Event.Data; e != nil {
			appendOutput(&stdout, e.Stdout)
			appendOutput(&stderr, e.Stderr)
		}
		if e := message.Event.End; e != nil {
			ended = true
			result.ExitCode = e.ExitCode
			if !e.Exited {
				return nil, fmt.Errorf("e2b process failed: %s", e.Error)
			}
		}
	}
}
func (s *runtime) filePath(path string) string {
	return "/files?path=" + url.QueryEscape(path) + "&username=" + url.QueryEscape(s.user)
}
func (s *runtime) Read(ctx context.Context, path string) (io.ReadCloser, error) {
	r, err := s.api.Do(ctx, "GET", s.filePath(path), nil, "")
	if err != nil {
		return nil, err
	}
	return r.Body, nil
}
func (s *runtime) Write(ctx context.Context, path string, content io.Reader) error {
	// Multipart is envd's native upload protocol. MultiReader streams the file
	// without a buffering goroutine or holding its content in memory.
	var envelope bytes.Buffer
	writer := multipart.NewWriter(&envelope)
	if _, err := writer.CreateFormFile("file", path); err != nil {
		return err
	}
	header := append([]byte(nil), envelope.Bytes()...)
	envelope.Reset()
	if err := writer.Close(); err != nil {
		return err
	}
	body := io.MultiReader(bytes.NewReader(header), content, bytes.NewReader(envelope.Bytes()))
	r, err := s.api.Do(ctx, "POST", s.filePath(path), body, writer.FormDataContentType())
	if err != nil {
		return err
	}
	return r.Body.Close()
}
func (s *runtime) Remove(ctx context.Context, path string) error {
	return s.api.JSON(ctx, "POST", "/filesystem.Filesystem/Remove", map[string]string{"path": path}, nil)
}

var _ sandbox.Provider = (*Provider)(nil)
