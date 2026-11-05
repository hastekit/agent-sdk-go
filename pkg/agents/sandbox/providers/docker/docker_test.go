package docker

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox/internal/provision"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

const containerID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// execCall is what the Engine received for one exec, and how the fixture answers.
type execCall struct {
	create container.ExecCreateRequest
	stdin  []byte
	closed bool
}

type execResult struct {
	stdout, stderr string
	code           int
}

// sweepContainer is one running container as the idle sweep sees it. A nil
// activity is a container whose activity file does not exist.
type sweepContainer struct {
	started  time.Time
	activity *time.Time
	execs    map[string]bool
	labels   map[string]string
}

type engineFixture struct {
	mu                       sync.Mutex
	config                   *container.Config
	host                     *container.HostConfig
	exists, running          bool
	creates, starts, deletes int
	pulls                    int
	imageStatus, execStatus  int
	pullError                bool
	name                     string
	sweep                    map[string]sweepContainer
	listQuery                string
	stopped                  []string
	calls                    []*execCall
	// readStdin says which execs get no output until their stdin closes.
	readStdin   func(*execCall) bool
	onExec      func(*execCall) execResult
	execResults map[string]int
}

func (f *engineFixture) execCalls() []*execCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*execCall(nil), f.calls...)
}

// serveExecStart hijacks the connection the way the Engine does, and frames output
// the way it does for a process without a TTY.
func (f *engineFixture) serveExecStart(w http.ResponseWriter, r *http.Request, id string) {
	f.mu.Lock()
	index, _ := strconv.Atoi(strings.TrimPrefix(id, "exec-"))
	call := f.calls[index]
	readStdin, onExec := f.readStdin != nil && f.readStdin(call), f.onExec
	f.mu.Unlock()
	io.Copy(io.Discard, r.Body)
	conn, buffered, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	io.WriteString(conn, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.multiplexed-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
	if readStdin {
		stdin, _ := io.ReadAll(buffered)
		f.mu.Lock()
		call.stdin, call.closed = stdin, true
		f.mu.Unlock()
	}
	result := execResult{}
	if onExec != nil {
		result = onExec(call)
	}
	f.mu.Lock()
	f.execResults[id] = result.code
	f.mu.Unlock()
	writeFrame(conn, stdcopy.Stdout, result.stdout)
	writeFrame(conn, stdcopy.Stderr, result.stderr)
}

func writeFrame(w io.Writer, stream stdcopy.StdType, data string) {
	if data == "" {
		return
	}
	header := [8]byte{0: byte(stream)}
	binary.BigEndian.PutUint32(header[4:], uint32(len(data)))
	w.Write(append(header[:], data...))
}

func (f *engineFixture) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1.52")
	if r.Method == "POST" && strings.HasPrefix(path, "/exec/") && strings.HasSuffix(path, "/start") {
		f.serveExecStart(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/exec/"), "/start"))
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	write := func(value any) { w.Header().Set("Content-Type", "application/json"); json.NewEncoder(w).Encode(value) }
	fail := func(status int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]string{"message": "engine failure"})
	}
	switch {
	case r.Method == "POST" && strings.HasSuffix(path, "/exec"):
		if f.execStatus != 0 {
			fail(f.execStatus)
			return
		}
		call := &execCall{}
		if err := json.NewDecoder(r.Body).Decode(&call.create); err != nil {
			fail(400)
			return
		}
		f.calls = append(f.calls, call)
		write(map[string]string{"Id": "exec-" + strconv.Itoa(len(f.calls)-1)})
	case r.Method == "GET" && strings.HasPrefix(path, "/exec/"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/exec/"), "/json")
		for _, c := range f.sweep {
			if running, ok := c.execs[id]; ok {
				code := 0
				write(container.ExecInspectResponse{ID: id, Running: running, ExitCode: &code})
				return
			}
		}
		code := f.execResults[id]
		write(container.ExecInspectResponse{ID: id, ExitCode: &code})
	case r.Method == "GET" && path == "/containers/json":
		f.listQuery = r.URL.RawQuery
		var items []container.Summary
		for id, c := range f.sweep {
			items = append(items, container.Summary{ID: id, Labels: c.labels})
		}
		write(items)
	case r.Method == "HEAD" && strings.HasSuffix(path, "/archive"):
		c := f.sweep[strings.Split(path, "/")[2]]
		if c.activity == nil {
			w.WriteHeader(404)
			return
		}
		stat, _ := json.Marshal(container.PathStat{Name: ".hastekit-activity", Mtime: *c.activity})
		w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString(stat))
	case r.Method == "POST" && strings.HasSuffix(path, "/stop"):
		f.stopped = append(f.stopped, strings.Split(path, "/")[2])
		w.WriteHeader(204)
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
		if c, ok := f.sweep[strings.Split(path, "/")[2]]; ok {
			var execIDs []string
			for id := range c.execs {
				execIDs = append(execIDs, id)
			}
			write(container.InspectResponse{ID: strings.Split(path, "/")[2], Config: &container.Config{}, ExecIDs: execIDs, State: &container.State{Status: "running", Running: true, StartedAt: c.started.Format(time.RFC3339Nano)}})
			return
		}
		if !f.exists {
			fail(404)
			return
		}
		status := container.ContainerState("created")
		if f.running {
			status = "running"
		}
		write(container.InspectResponse{ID: containerID, Config: f.config, HostConfig: f.host, State: &container.State{Status: status, Running: f.running}, NetworkSettings: &container.NetworkSettings{Ports: network.PortMap{
			network.MustParsePort("9000/tcp"): {{HostPort: "39000"}},
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
	f.execResults = map[string]int{}
	engine := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(engine.Close)
	engineClient, err := client.New(client.WithHost(strings.Replace(engine.URL, "http://", "tcp://", 1)), client.WithAPIVersion("1.52"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engineClient.Close() })
	cfg := Config{Client: engineClient, Profiles: map[string]Profile{"base": {Config: container.Config{Image: "sandbox-image"}}}, ReadyTimeout: time.Second}
	if configure != nil {
		configure(&cfg)
	}
	provider, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { provider.Close() })
	return provider
}
func createRequest() sandbox.CreateRequest {
	return sandbox.CreateRequest{Profile: "base", Session: sandbox.SessionKey{Namespace: "project", SessionID: "session"}, Env: map[string]string{"TOKEN": "secret"}}
}

func TestLifecycleConflictAndExec(t *testing.T) {
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
	labels, _ := json.Marshal(f.config.Labels)
	if strings.Contains(string(labels), "secret") {
		t.Error("credentials in labels")
	}
	f.onExec = func(*execCall) execResult { return execResult{stdout: "out", stderr: "err", code: 7} }
	f.mu.Unlock()
	result, err := sb.Exec(ctx, sandbox.ExecRequest{Argv: []string{"echo", "$(hello)"}, Env: map[string]string{"KEY": "a value"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "out" || result.Stderr != "err" || result.ExitCode != 7 {
		t.Fatalf("%+v", result)
	}
	calls := f.execCalls()
	last := calls[len(calls)-1].create
	if !last.AttachStdin || !last.AttachStdout || !last.AttachStderr || last.Tty {
		t.Fatalf("exec streams: %+v", last)
	}
	if got := last.Cmd[len(last.Cmd)-4:]; got[0] != "env" || got[1] != "KEY=a value" || got[2] != "echo" || got[3] != "$(hello)" {
		t.Fatalf("argv changed: %v", last.Cmd)
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
	f.running, f.onExec = false, nil
	f.mu.Unlock()
	if deletes != 0 {
		t.Fatal("conflict deleted the winner")
	}
	if _, err := p.Connect(ctx, sb.Reference()); err != nil {
		t.Fatal("reconnect stopped container", err)
	}
	f.mu.Lock()
	starts := f.starts
	f.mu.Unlock()
	if starts != 2 {
		t.Fatal("reconnect did not start the stopped container", starts)
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

// Nothing of ours runs in the container, so nothing needs a port, and the
// container has to be kept alive by something that stops on its own.
func TestContainerNeedsNoDaemonOrPort(t *testing.T) {
	f := &engineFixture{}
	p := fixtureProvider(t, f, func(cfg *Config) {
		cfg.MaxLifetime = 90 * time.Minute
		cfg.Profiles["none"] = Profile{Config: container.Config{Image: "sandbox-image"}, HostConfig: container.HostConfig{NetworkMode: "none"}}
	})
	req := createRequest()
	req.Profile = "none"
	if _, err := p.Create(context.Background(), req); err != nil {
		t.Fatal("networking disabled:", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := strings.Join(f.config.Entrypoint, " "); got != "sleep 5400" {
		t.Fatalf("entrypoint %q", got)
	}
	if f.host.Init == nil || !*f.host.Init {
		t.Fatal("no init to reap processes")
	}
	if len(f.config.ExposedPorts) != 0 || len(f.host.PortBindings) != 0 {
		t.Fatalf("ports: %v %v", f.config.ExposedPorts, f.host.PortBindings)
	}
	for _, entry := range f.config.Env {
		if strings.HasPrefix(entry, "SANDBOX_PORT=") {
			t.Fatal(entry)
		}
	}
}

func TestProfileKeepsItsOwnEntrypointAndInit(t *testing.T) {
	f := &engineFixture{}
	init := false
	p := fixtureProvider(t, f, func(cfg *Config) {
		cfg.Profiles["custom"] = Profile{Config: container.Config{Image: "sandbox-image", Entrypoint: []string{"my-init"}}, HostConfig: container.HostConfig{Init: &init}}
	})
	req := createRequest()
	req.Profile = "custom"
	if _, err := p.Create(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Join(f.config.Entrypoint, " ") != "my-init" || f.host.Init == nil || *f.host.Init {
		t.Fatalf("entrypoint %v, init %v", f.config.Entrypoint, f.host.Init)
	}
}

const startupScript = "start-my-service"

func withStartupScript(cfg *Config) {
	profile := cfg.Profiles["base"]
	profile.StartupScript = startupScript
	cfg.Profiles["base"] = profile
}

func countStartups(f *engineFixture) int {
	count := 0
	for _, call := range f.execCalls() {
		if strings.Contains(strings.Join(call.create.Cmd, " "), startupScript) {
			count++
		}
	}
	return count
}

// A restart loses every process, so the script runs on each start, and only then.
func TestStartupScriptRunsWhenAContainerStarts(t *testing.T) {
	f := &engineFixture{}
	p := fixtureProvider(t, f, withStartupScript)
	ctx := context.Background()
	sb, err := p.Create(ctx, createRequest())
	if err != nil {
		t.Fatal(err)
	}
	if countStartups(f) != 1 {
		t.Fatal("did not run for a new container", countStartups(f))
	}
	if _, err := p.Connect(ctx, sb.Reference()); err != nil {
		t.Fatal(err)
	}
	if countStartups(f) != 1 {
		t.Fatal("ran again for a running container", countStartups(f))
	}
	f.mu.Lock()
	f.running = false
	f.mu.Unlock()
	if _, err := p.Connect(ctx, sb.Reference()); err != nil {
		t.Fatal(err)
	}
	if countStartups(f) != 2 {
		t.Fatal("did not run for a restarted container", countStartups(f))
	}
}

func TestNoStartupScriptRunsNothing(t *testing.T) {
	f := &engineFixture{}
	p := fixtureProvider(t, f, nil)
	if _, err := p.Create(context.Background(), createRequest()); err != nil {
		t.Fatal(err)
	}
	if calls := f.execCalls(); len(calls) != 0 {
		t.Fatalf("ran %d commands", len(calls))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.config.Labels[provision.StartupScriptKey]; ok {
		t.Fatal("empty script stored")
	}
}

// The next Connect only runs the script for a container it has to start, so one
// left running after a failure would never get another try.
func TestFailedStartupOnRestartStopsTheContainer(t *testing.T) {
	f := &engineFixture{}
	p := fixtureProvider(t, f, withStartupScript)
	ctx := context.Background()
	sb, err := p.Create(ctx, createRequest())
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.running = false
	f.onExec = func(*execCall) execResult { return execResult{stderr: "registry unreachable", code: 3} }
	f.mu.Unlock()
	if _, err := p.Connect(ctx, sb.Reference()); err == nil || !strings.Contains(err.Error(), "registry unreachable") {
		t.Fatalf("err=%v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.stopped) != 1 || f.deletes != 0 {
		t.Fatalf("stopped %v, deleted %d", f.stopped, f.deletes)
	}
}

func TestFailedStartupRemovesANewContainer(t *testing.T) {
	f := &engineFixture{onExec: func(*execCall) execResult { return execResult{stderr: "registry unreachable", code: 3} }}
	p := fixtureProvider(t, f, withStartupScript)
	if _, err := p.Create(context.Background(), createRequest()); err == nil || !strings.Contains(err.Error(), "registry unreachable") {
		t.Fatalf("err=%v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deletes != 1 {
		t.Fatal("failed container left behind", f.deletes)
	}
}

// The script is part of what was asked for, so a different one is a different sandbox.
func TestChangedStartupScriptIsAConflict(t *testing.T) {
	f := &engineFixture{}
	p := fixtureProvider(t, f, withStartupScript)
	ctx := context.Background()
	if _, err := p.Create(ctx, createRequest()); err != nil {
		t.Fatal(err)
	}
	profile := p.cfg.Profiles["base"]
	profile.StartupScript = "something-else"
	p.cfg.Profiles["base"] = profile
	if _, err := p.Create(ctx, createRequest()); !errors.Is(err, sandbox.ErrConflict) {
		t.Fatalf("err=%v", err)
	}
}

// A process learns its caller has gone from EOF on stdin, so cancelling has to
// close the connection and not merely stop reading from it.
func TestCancelClosesTheConnection(t *testing.T) {
	f := &engineFixture{}
	p := fixtureProvider(t, f, nil)
	sb, err := p.Create(context.Background(), createRequest())
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.readStdin = func(*execCall) bool { return true }
	f.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := sb.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sleep", "60"}})
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not end the exec")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		calls := f.execCalls()
		f.mu.Lock()
		closed := calls[len(calls)-1].closed
		f.mu.Unlock()
		if closed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the Engine never saw the connection close")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStopIdleStopsOnlyWhatIsIdle(t *testing.T) {
	now := time.Now()
	old, recent := now.Add(-2*time.Hour), now.Add(-time.Minute)
	f := &engineFixture{sweep: map[string]sweepContainer{
		"idle":           {started: old, activity: &old},
		"no-activity":    {started: old},
		"used":           {started: old, activity: &recent},
		"just-started":   {started: recent, activity: &old},
		"busy":           {started: old, activity: &old, execs: map[string]bool{"finished": false, "running": true}},
		"finished-execs": {started: old, activity: &old, execs: map[string]bool{"done": false}},
		"kubernetes-pod": {started: old, activity: &old, labels: map[string]string{kubernetesPodLabel: "a-pod"}},
	}}
	p := fixtureProvider(t, f, func(cfg *Config) { cfg.IdleTimeout = -1 })
	stopped, err := p.StopIdle(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	got := map[string]bool{}
	for _, id := range f.stopped {
		got[id] = true
	}
	if stopped != 3 || len(got) != 3 || !got["idle"] || !got["no-activity"] || !got["finished-execs"] {
		t.Fatalf("stopped %d: %v", stopped, f.stopped)
	}
	if !strings.Contains(f.listQuery, "hastekit.managed") || !strings.Contains(f.listQuery, "running") {
		t.Fatalf("listed more than our running containers: %s", f.listQuery)
	}
}

func TestProviderSweepsOnItsOwn(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	f := &engineFixture{sweep: map[string]sweepContainer{"idle": {started: old, activity: &old}}}
	p := fixtureProvider(t, f, func(cfg *Config) { cfg.IdleTimeout = time.Millisecond })
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		stopped := len(f.stopped)
		f.mu.Unlock()
		if stopped > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("nothing stopped the idle container")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNegativeIdleTimeoutLeavesStoppingToTheCaller(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	f := &engineFixture{sweep: map[string]sweepContainer{"idle": {started: old, activity: &old}}}
	p := fixtureProvider(t, f, func(cfg *Config) { cfg.IdleTimeout = -1 })
	if p.stopSweep != nil {
		t.Fatal("started a sweep")
	}
	if _, err := p.StopIdle(context.Background(), -time.Second); err == nil {
		t.Fatal("accepted a negative idle duration")
	}
}

func TestConfigRejectsNegativeMaxLifetime(t *testing.T) {
	if _, err := New(Config{Client: &client.Client{}, MaxLifetime: -time.Second}); err == nil {
		t.Fatal("accepted a negative max lifetime")
	}
}

func TestImagePullErrorsAndReadinessCleanup(t *testing.T) {
	for _, scenario := range []string{"pull-success", "pull-error", "image-auth-error", "new-unusable", "existing-unusable"} {
		t.Run(scenario, func(t *testing.T) {
			f := &engineFixture{}
			switch scenario {
			case "pull-success", "pull-error":
				f.imageStatus = 404
				f.pullError = scenario == "pull-error"
			case "image-auth-error":
				f.imageStatus = 401
			case "new-unusable":
				f.execStatus = 500
			}
			p := fixtureProvider(t, f, func(cfg *Config) {
				cfg.ReadyTimeout = 200 * time.Millisecond
				withStartupScript(cfg)
			})
			req := createRequest()
			if scenario == "existing-unusable" {
				if _, err := p.Create(context.Background(), req); err != nil {
					t.Fatal(err)
				}
				f.mu.Lock()
				f.running, f.execStatus = false, 500
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
			if scenario == "new-unusable" && f.deletes != 1 {
				t.Fatal("failed owned container not cleaned up")
			}
			if scenario == "existing-unusable" && f.deletes != 0 {
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
			return "http://proxy.test:" + strconv.Itoa(port), nil
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
	sb, err := p.Create(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := sb.(sandbox.EndpointResolver).Endpoint(context.Background(), 9000)
	if err != nil || endpoint != "http://proxy.test:9000" {
		t.Fatalf("custom endpoint: %s %v", endpoint, err)
	}
}
