// Package docker provisions sandboxes through the Docker Engine API and runs
// commands in them with its exec API. Nothing of ours runs inside the container.
package docker

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox/internal/execsandbox"
	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox/internal/provision"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// Engine is implemented by the official Docker client. Inject one to select a
// remote engine, TLS credentials, API settings, or a test transport.
type Engine interface {
	ContainerCreate(context.Context, client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
	ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerStop(context.Context, string, client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerStatPath(context.Context, string, client.ContainerStatPathOptions) (client.ContainerStatPathResult, error)
	ExecCreate(context.Context, string, client.ExecCreateOptions) (client.ExecCreateResult, error)
	ExecAttach(context.Context, string, client.ExecAttachOptions) (client.ExecAttachResult, error)
	ExecInspect(context.Context, string, client.ExecInspectOptions) (client.ExecInspectResult, error)
	ImageInspect(context.Context, string, ...client.ImageInspectOption) (client.ImageInspectResult, error)
	ImagePull(context.Context, string, client.ImagePullOptions) (client.ImagePullResponse, error)
}

type PullPolicy string

const (
	PullIfMissing PullPolicy = "missing"
	PullAlways    PullPolicy = "always"
	PullNever     PullPolicy = "never"
)

// Profile configures the container using native Docker API types. Config.Image
// is required, and the image must provide a POSIX shell and the tools listed in
// the README. The default entrypoint only keeps the container running; one that
// replaces it must too.
type Profile struct {
	Config           container.Config
	HostConfig       container.HostConfig
	NetworkingConfig network.NetworkingConfig
	PullPolicy       PullPolicy
	RegistryAuth     string // Docker's base64-encoded registry authentication, never stored in labels/state.
	// StartupScript runs in sh each time a container starts, which includes being
	// started again after going idle, so it has to be safe to run again. It is
	// stored in a label, so keep secrets out of it. It shares ReadyTimeout, and a
	// container whose script fails is not used.
	StartupScript string
}

type Config struct {
	Client        Engine // nil creates an official client using Docker environment variables.
	Profiles      map[string]Profile
	ReadyTimeout  time.Duration // includes image pull, startup and the startup script; defaults to two minutes.
	PublishedHost string        // default 127.0.0.1, reachable host for published service ports
	// IdleTimeout stops a running container after this long without activity. It
	// defaults to 15 minutes, and a negative value leaves stopping to the caller,
	// through StopIdle. Every provider sharing an engine sweeps its containers,
	// so the shortest timeout among them wins.
	IdleTimeout time.Duration
	// MaxLifetime stops a container this long after it started no matter what,
	// which bounds its lifetime when no provider is left to notice it is idle.
	// Connect starts it again. Defaults to 24 hours.
	MaxLifetime time.Duration
	// Configure runs on an isolated profile copy. Use it for session-specific
	// bind mounts, volumes, networking or resources. Name and ownership labels
	// are assigned afterwards. It must produce stable configuration for reuse.
	Configure func(context.Context, sandbox.CreateRequest, *client.ContainerCreateOptions) error
	// Endpoint overrides published-port routing for service ports requested
	// through sandbox.EndpointResolver.
	Endpoint func(context.Context, container.InspectResponse, int) (string, error)
}

type Provider struct {
	cfg        Config
	engine     Engine
	close      func() error
	stopSweep  context.CancelFunc
	sweepEnded chan struct{}
}

func New(cfg Config) (*Provider, error) {
	if cfg.ReadyTimeout == 0 {
		cfg.ReadyTimeout = 2 * time.Minute
	}
	if cfg.ReadyTimeout < 0 {
		return nil, fmt.Errorf("negative readiness timeout")
	}
	if cfg.PublishedHost == "" {
		cfg.PublishedHost = "127.0.0.1"
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 15 * time.Minute
	}
	if cfg.MaxLifetime == 0 {
		cfg.MaxLifetime = 24 * time.Hour
	}
	if cfg.MaxLifetime < 0 {
		return nil, fmt.Errorf("negative max lifetime")
	}
	profiles, err := provision.Clone(cfg.Profiles)
	if err != nil {
		return nil, err
	}
	cfg.Profiles = profiles
	for name, profile := range profiles {
		if profile.Config.Image == "" {
			return nil, fmt.Errorf("docker profile %q requires Config.Image", name)
		}
		switch profile.PullPolicy {
		case "", PullIfMissing, PullAlways, PullNever:
		default:
			return nil, fmt.Errorf("invalid image pull policy for %q", name)
		}
	}
	p := &Provider{cfg: cfg, engine: cfg.Client}
	if p.engine == nil {
		engine, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			return nil, err
		}
		p.engine, p.close = engine, engine.Close
	}
	if cfg.IdleTimeout > 0 {
		var ctx context.Context
		ctx, p.stopSweep = context.WithCancel(context.Background())
		p.sweepEnded = make(chan struct{})
		go p.sweep(ctx)
	}
	return p, nil
}

// Close stops the idle sweep and closes only the internally constructed Engine
// client. An injected client remains owned by its caller.
func (p *Provider) Close() error {
	if p.stopSweep != nil {
		p.stopSweep()
		<-p.sweepEnded
	}
	if p.close != nil {
		return p.close()
	}
	return nil
}

func (p *Provider) options(ctx context.Context, req sandbox.CreateRequest) (client.ContainerCreateOptions, Profile, error) {
	profile, ok := p.cfg.Profiles[req.Profile]
	if !ok {
		return client.ContainerCreateOptions{}, profile, fmt.Errorf("unknown docker profile %q", req.Profile)
	}
	profile, err := provision.Clone(profile)
	if err != nil {
		return client.ContainerCreateOptions{}, profile, err
	}
	options := client.ContainerCreateOptions{Config: &profile.Config, HostConfig: &profile.HostConfig, NetworkingConfig: &profile.NetworkingConfig}
	if options.Config.WorkingDir == "" {
		options.Config.WorkingDir = "/workspace"
	}
	if len(options.Config.Entrypoint) == 0 {
		options.Config.Entrypoint = []string{"sleep", strconv.FormatInt(int64((p.cfg.MaxLifetime+time.Second-1)/time.Second), 10)}
	}
	env := map[string]string{}
	for _, entry := range options.Config.Env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			return options, profile, fmt.Errorf("invalid profile environment")
		}
		env[key] = value
	}
	for key, value := range req.Env {
		env[key] = value
	}
	env["SANDBOX_ROOT"] = options.Config.WorkingDir
	options.Config.Env = nil
	for key, value := range env {
		options.Config.Env = append(options.Config.Env, key+"="+value)
	}
	sort.Strings(options.Config.Env)
	if p.cfg.Configure != nil {
		if err := p.cfg.Configure(ctx, req, &options); err != nil {
			return options, profile, err
		}
	}
	if options.Config == nil || options.Config.Image == "" || options.Image != "" {
		return options, profile, fmt.Errorf("Config.Image is required; Image shortcut is unsupported")
	}
	if options.HostConfig == nil {
		options.HostConfig = &container.HostConfig{}
	}
	// PID 1 reaps what commands leave behind and passes on the stop signal.
	if options.HostConfig.Init == nil {
		init := true
		options.HostConfig.Init = &init
	}
	options.Name = "hastekit-" + sandbox.ResourceID(req.Session)
	if options.Config.Labels == nil {
		options.Config.Labels = map[string]string{}
	}
	delete(options.Config.Labels, provision.FingerprintKey)
	options.Config.Labels[provision.ManagedKey] = provision.ManagedValue
	delete(options.Config.Labels, provision.StartupScriptKey)
	if profile.StartupScript != "" {
		options.Config.Labels[provision.StartupScriptKey] = profile.StartupScript
	}
	fingerprint, err := provision.Fingerprint(req, options)
	if err != nil {
		return options, profile, err
	}
	options.Config.Labels[provision.FingerprintKey] = fingerprint
	return options, profile, nil
}

func (p *Provider) Create(ctx context.Context, req sandbox.CreateRequest) (sandbox.Sandbox, error) {
	if err := provision.Validate(req); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.ReadyTimeout)
	defer cancel()
	options, profile, err := p.options(ctx, req)
	if err != nil {
		return nil, err
	}
	if err = p.pull(ctx, options.Config.Image, profile); err != nil {
		return nil, err
	}
	created, err := p.engine.ContainerCreate(ctx, options)
	if errdefs.IsConflict(err) {
		existing, lookupErr := p.engine.ContainerInspect(ctx, options.Name, client.ContainerInspectOptions{})
		if lookupErr != nil {
			return nil, errors.Join(mapError(err), mapError(lookupErr))
		}
		config := existing.Container.Config
		if config == nil || config.Labels[provision.ManagedKey] != provision.ManagedValue || config.Labels[provision.FingerprintKey] != options.Config.Labels[provision.FingerprintKey] {
			return nil, fmt.Errorf("%w: named container has different configuration", sandbox.ErrConflict)
		}
		return p.Connect(ctx, sandbox.Reference{Provider: "docker", ID: existing.Container.ID})
	}
	if err != nil {
		return nil, mapError(err)
	}
	ref := sandbox.Reference{Provider: "docker", ID: created.ID}
	if err := validate(ref); err != nil {
		return nil, err
	}
	result, err := p.Connect(ctx, ref)
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_, removeErr := p.engine.ContainerRemove(cleanup, created.ID, client.ContainerRemoveOptions{Force: true})
		if !errdefs.IsNotFound(removeErr) {
			err = errors.Join(err, mapError(removeErr))
		}
	}
	return result, err
}

func (p *Provider) pull(ctx context.Context, image string, profile Profile) error {
	if profile.PullPolicy == PullNever {
		return nil
	}
	if profile.PullPolicy != PullAlways {
		_, err := p.engine.ImageInspect(ctx, image)
		if err == nil {
			return nil
		}
		if !errdefs.IsNotFound(err) {
			return mapError(err)
		}
	}
	response, err := p.engine.ImagePull(ctx, image, client.ImagePullOptions{RegistryAuth: profile.RegistryAuth})
	if err != nil {
		return mapError(err)
	}
	defer response.Close()
	return response.Wait(ctx)
}

func validate(ref sandbox.Reference) error {
	if ref.Provider != "docker" || len(ref.ID) != 64 {
		return fmt.Errorf("invalid docker reference: immutable container ID required")
	}
	if _, err := hex.DecodeString(ref.ID); err != nil {
		return fmt.Errorf("invalid docker container ID")
	}
	return nil
}
func mapError(err error) error {
	if errdefs.IsNotFound(err) {
		return fmt.Errorf("%w: %w", sandbox.ErrNotFound, err)
	}
	if errdefs.IsConflict(err) {
		return fmt.Errorf("%w: %w", sandbox.ErrConflict, err)
	}
	return err
}

func (p *Provider) Connect(ctx context.Context, ref sandbox.Reference) (sandbox.Sandbox, error) {
	if err := validate(ref); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.ReadyTimeout)
	defer cancel()
	started := false
	for {
		inspected, err := p.engine.ContainerInspect(ctx, ref.ID, client.ContainerInspectOptions{})
		if err != nil {
			return nil, mapError(err)
		}
		info := inspected.Container
		if info.ID != ref.ID || info.Config == nil || info.Config.Labels[provision.ManagedKey] != provision.ManagedValue {
			return nil, fmt.Errorf("%w: container identity or ownership mismatch", sandbox.ErrConflict)
		}
		if info.State == nil {
			return nil, fmt.Errorf("docker returned no container state")
		}
		if info.State.Dead || info.State.Paused || info.State.Status == "removing" {
			return nil, fmt.Errorf("%w: container is %s", sandbox.ErrConflict, info.State.Status)
		}
		if info.State.Running && !info.State.Restarting {
			result := &runtime{Sandbox: execsandbox.New(execsandbox.Config{Reference: ref, Runner: &runner{engine: p.engine, id: ref.ID}, Root: info.Config.WorkingDir}), provider: p}
			if started {
				if err := result.Startup(ctx, info.Config.Labels[provision.StartupScriptKey], p.cfg.ReadyTimeout); err != nil {
					// Left running, it would look ready to the next Connect, which would
					// skip the script.
					cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
					defer cancel()
					_, stopErr := p.engine.ContainerStop(cleanup, ref.ID, client.ContainerStopOptions{})
					return nil, errors.Join(err, mapError(stopErr))
				}
			}
			return result, nil
		}
		if !started && !info.State.Restarting {
			_, err := p.engine.ContainerStart(ctx, ref.ID, client.ContainerStartOptions{})
			if err != nil && !errdefs.IsConflict(err) {
				return nil, mapError(err)
			}
			started = true
		} else if started && !info.State.Restarting && (info.State.Status == "exited" || info.State.Status == "dead") {
			return nil, fmt.Errorf("sandbox container exited during startup (exit %d): %s", info.State.ExitCode, info.State.Error)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (p *Provider) endpoint(ctx context.Context, info container.InspectResponse, port int) (string, error) {
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("invalid service port")
	}
	if p.cfg.Endpoint != nil {
		return p.cfg.Endpoint(ctx, info, port)
	}
	if info.NetworkSettings != nil {
		bindings := info.NetworkSettings.Ports[network.MustParsePort(strconv.Itoa(port)+"/tcp")]
		if len(bindings) > 0 && bindings[0].HostPort != "" {
			return "http://" + net.JoinHostPort(p.cfg.PublishedHost, bindings[0].HostPort), nil
		}
	}
	return "", fmt.Errorf("%w: port %d is not published; configure port bindings or Endpoint", sandbox.ErrUnsupported, port)
}
func (p *Provider) Delete(ctx context.Context, ref sandbox.Reference) error {
	if err := validate(ref); err != nil {
		return err
	}
	_, err := p.engine.ContainerRemove(ctx, ref.ID, client.ContainerRemoveOptions{Force: true})
	if errdefs.IsNotFound(err) {
		return nil
	}
	return mapError(err)
}

type runtime struct {
	*execsandbox.Sandbox
	provider *Provider
}

func (s *runtime) Endpoint(ctx context.Context, port int) (string, error) {
	info, err := s.provider.engine.ContainerInspect(ctx, s.Reference().ID, client.ContainerInspectOptions{})
	if err != nil {
		return "", mapError(err)
	}
	return s.provider.endpoint(ctx, info.Container, port)
}

var _ sandbox.Provider = (*Provider)(nil)
