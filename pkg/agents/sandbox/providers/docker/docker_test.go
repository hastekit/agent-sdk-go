package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

const containerID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type engineFixture struct {
	mu                              sync.Mutex
	config                          *container.Config
	host                            *container.HostConfig
	exists, running, unhealthy      bool
	creates, starts, deletes, pulls int
	imageStatus                     int
	pullError                       bool
	daemonPort                      string
	name                            string
}

func (f *engineFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/v1.52")
	write := func(value any) { w.Header().Set("Content-Type", "application/json"); json.NewEncoder(w).Encode(value) }
	fail := func(status int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]string{"message": "engine failure"})
	}
	switch {
	case r.Method == "GET" && strings.HasPrefix(path, "/images/"):
		if f.imageStatus != 0 {
			fail(f.imageStatus)
			return
		}
		write(map[string]string{"Id": "image"})
	case path == "/images/create":
		f.pulls++
		if f.pullError {
			write(map[string]any{"error": "pull denied", "errorDetail": map[string]string{"message": "pull denied"}})
			return
		}
		write(map[string]string{"status": "complete"})
	case path == "/containers/create":
		f.creates++
		if f.exists {
			fail(409)
			return
		}
		var body container.CreateRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fail(400)
			return
		}
		f.config, f.host, f.exists, f.name = body.Config, body.HostConfig, true, r.URL.Query().Get("name")
		write(map[string]any{"Id": containerID, "Warnings": []string{}})
	case r.Method == "GET" && strings.HasSuffix(path, "/json"):
		if !f.exists {
			fail(404)
			return
		}
		status := container.ContainerState("created")
		if f.running {
			status = "running"
		}
		write(container.InspectResponse{ID: containerID, Config: f.config, HostConfig: f.host, State: &container.State{Status: status, Running: f.running}, NetworkSettings: &container.NetworkSettings{Ports: network.PortMap{
			network.MustParsePort("8080/tcp"): {{HostPort: f.daemonPort}}, network.MustParsePort("9000/tcp"): {{HostPort: "39000"}},
		}}})
	case strings.HasSuffix(path, "/start"):
		f.starts++
		f.running = true
		w.WriteHeader(204)
	case r.Method == "DELETE":
		f.deletes++
		f.exists = false
		w.WriteHeader(204)
	default:
		fail(404)
	}
}
func fixtureProvider(t *testing.T, f *engineFixture, configure func(*Config)) *Provider {
	t.Helper()
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		unhealthy := f.unhealthy
		f.mu.Unlock()
		if unhealthy {
			w.WriteHeader(503)
			return
		}
		if r.URL.Path == "/health" {
			w.Write([]byte("ok"))
			return
		}
		if r.URL.Path == "/v2/exec" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"stdout":"daemon","exit_code":7}`))
			return
		}
		if r.URL.Path == "/v2/files" {
			if r.Method == "PUT" {
				data, _ := io.ReadAll(r.Body)
				if !bytes.Equal(data, []byte{0, 255, 10}) {
					t.Error("binary upload changed")
				}
				w.WriteHeader(204)
				return
			}
			w.Write([]byte{0, 255, 10})
			return
		}
		t.Errorf("unexpected daemon route: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(404)
	}))
	t.Cleanup(daemon.Close)
	u, _ := url.Parse(daemon.URL)
	f.daemonPort = u.Port()
	engine := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(engine.Close)
	engineClient, err := client.New(client.WithHost(engine.URL), client.WithAPIVersion("1.52"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engineClient.Close() })
	cfg := Config{Client: engineClient, Profiles: map[string]Profile{"base": {Config: container.Config{Image: "daemon-image"}}}, ReadyTimeout: time.Second}
	if configure != nil {
		configure(&cfg)
	}
	provider, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}
func createRequest() sandbox.CreateRequest {
	return sandbox.CreateRequest{Profile: "base", Session: sandbox.SessionKey{Namespace: "project", SessionID: "session"}, Env: map[string]string{"TOKEN": "secret"}}
}

func TestLifecycleConflictAndDaemon(t *testing.T) {
	f := &engineFixture{}
	p := fixtureProvider(t, f, func(cfg *Config) {
		cfg.Configure = func(_ context.Context, req sandbox.CreateRequest, opts *client.ContainerCreateOptions) error {
			opts.HostConfig.Mounts = []mount.Mount{{Type: mount.TypeBind, Source: "/uploads/" + req.Session.SessionID, Target: "/workspace/uploads", ReadOnly: true}}
			return nil
		}
	})
	ctx := context.Background()
	req := createRequest()
	sb, err := p.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Reference().ID != containerID {
		t.Fatal("reference is not immutable container ID")
	}
	f.mu.Lock()
	if f.name != "hastekit-"+sandbox.ResourceID(req.Session) || len(f.host.Mounts) != 1 || f.host.Mounts[0].Source != "/uploads/session" {
		t.Error("customized configuration lost")
	}
	if len(f.host.PortBindings[network.MustParsePort("8080/tcp")]) != 1 {
		t.Error("daemon port not published")
	}
	labels, _ := json.Marshal(f.config.Labels)
	if strings.Contains(string(labels), "secret") {
		t.Error("credentials in labels")
	}
	f.mu.Unlock()
	result, err := sb.Exec(ctx, sandbox.ExecRequest{Argv: []string{"echo", "hello"}})
	if err != nil || result.ExitCode != 7 {
		t.Fatalf("%+v %v", result, err)
	}
	if err := sb.Files().Write(ctx, "file.bin", bytes.NewReader([]byte{0, 255, 10})); err != nil {
		t.Fatal(err)
	}
	reader, err := sb.Files().Read(ctx, "file.bin")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(reader)
	reader.Close()
	if !bytes.Equal(data, []byte{0, 255, 10}) {
		t.Fatal("binary download changed")
	}
	endpoint, err := sb.(sandbox.EndpointResolver).Endpoint(ctx, 9000)
	if err != nil || endpoint != "http://127.0.0.1:39000" {
		t.Fatalf("published endpoint: %s %v", endpoint, err)
	}
	reused, err := p.Create(ctx, req)
	if err != nil || reused.Reference() != sb.Reference() {
		t.Fatalf("conflict recovery: %v", err)
	}
	req.Env["TOKEN"] = "different"
	if _, err := p.Create(ctx, req); !errors.Is(err, sandbox.ErrConflict) {
		t.Fatalf("configuration conflict: %v", err)
	}
	f.mu.Lock()
	deletes := f.deletes
	f.running = false
	f.mu.Unlock()
	if deletes != 0 {
		t.Fatal("conflict deleted the winner")
	}
	if _, err := p.Connect(ctx, sb.Reference()); err != nil {
		t.Fatal("reconnect stopped container", err)
	}
	if err := p.Delete(ctx, sb.Reference()); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Connect(ctx, sb.Reference()); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatal(err)
	}
	f.mu.Lock()
	creates := f.creates
	f.mu.Unlock()
	if creates != 3 {
		t.Fatal("connect created replacement", creates)
	}
}

func TestImagePullErrorsAndReadinessCleanup(t *testing.T) {
	for _, scenario := range []string{"pull-success", "pull-error", "image-auth-error", "new-unhealthy", "existing-unhealthy"} {
		t.Run(scenario, func(t *testing.T) {
			f := &engineFixture{}
			switch scenario {
			case "pull-success", "pull-error":
				f.imageStatus = 404
				f.pullError = scenario == "pull-error"
			case "image-auth-error":
				f.imageStatus = 401
			case "new-unhealthy":
				f.unhealthy = true
			}
			p := fixtureProvider(t, f, func(cfg *Config) { cfg.ReadyTimeout = 200 * time.Millisecond })
			req := createRequest()
			if scenario == "existing-unhealthy" {
				if _, err := p.Create(context.Background(), req); err != nil {
					t.Fatal(err)
				}
				f.mu.Lock()
				f.unhealthy = true
				f.mu.Unlock()
			}
			_, err := p.Create(context.Background(), req)
			if scenario == "pull-success" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("expected error")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if scenario == "new-unhealthy" && f.deletes != 1 {
				t.Fatal("failed owned container not cleaned up")
			}
			if scenario == "existing-unhealthy" && f.deletes != 0 {
				t.Fatal("deleted existing container on failed reconnect")
			}
			if scenario == "pull-error" && (f.creates != 0 || f.pulls != 1) {
				t.Fatal("created after failed image pull")
			}
			if scenario == "image-auth-error" && (f.creates != 0 || f.pulls != 0) {
				t.Fatal("ignored registry inspection failure")
			}
		})
	}
}

func TestCustomEndpointAndProfileIsolation(t *testing.T) {
	f := &engineFixture{}
	p := fixtureProvider(t, f, func(cfg *Config) {
		cfg.Endpoint = func(_ context.Context, _ container.InspectResponse, port int) (string, error) {
			return "http://127.0.0.1:" + f.daemonPort, nil
		}
		cfg.Configure = func(_ context.Context, _ sandbox.CreateRequest, opts *client.ContainerCreateOptions) error {
			opts.Config.Env = append(opts.Config.Env, "CUSTOM=one")
			return nil
		}
	})
	req := createRequest()
	first, _, err := p.options(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := p.options(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Config.Env) != len(second.Config.Env) || len(p.cfg.Profiles["base"].Config.Env) != 0 {
		t.Fatal("profile mutated")
	}
	if len(first.HostConfig.PortBindings) != 0 {
		t.Fatal("custom endpoint unexpectedly exposed daemon")
	}
	if _, err := p.Create(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}
