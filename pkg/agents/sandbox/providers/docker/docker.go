// Package docker provisions daemon-backed sandboxes through the Docker Engine API.
package docker

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox/internal/daemonprovider"
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
	ImageInspect(context.Context, string, ...client.ImageInspectOption) (client.ImageInspectResult, error)
	ImagePull(context.Context, string, client.ImagePullOptions) (client.ImagePullResponse, error)
}

type PullPolicy string

const (
	PullIfMissing PullPolicy = "missing"
	PullAlways    PullPolicy = "always"
	PullNever     PullPolicy = "never"
)

// Profile configures the daemon container using native Docker API types.
// Config.Image is required. The default entrypoint is sandbox-daemon.
type Profile struct {
	Config           container.Config
	HostConfig       container.HostConfig
	NetworkingConfig network.NetworkingConfig
	PullPolicy       PullPolicy
	RegistryAuth     string // Docker's base64-encoded registry authentication, never stored in labels/state.
}

type Config struct {
	Client         Engine // nil creates an official client using Docker environment variables.
	Profiles       map[string]Profile
	DaemonPort     int           // defaults to 8080
	ReadyTimeout   time.Duration // includes image pull, startup, and daemon readiness; defaults to two minutes.
	HTTPClient     *http.Client  // daemon transport, independent of the Engine client
	PublishAddress string        // default 127.0.0.1, used for automatic daemon port publishing
	DaemonHost     string        // default 127.0.0.1, reachable host for published ports
	// Configure runs on an isolated profile copy. Use it for session-specific
	// bind mounts, volumes, networking or resources. Name and ownership labels
	// are assigned afterwards. It must produce stable configuration for reuse.
	Configure func(context.Context, sandbox.CreateRequest, *client.ContainerCreateOptions) error
	// Endpoint overrides published-port routing. With this set the provider does
	// not automatically publish the daemon port. The callback must resolve every
	// requested service port, including DaemonPort, to an address reachable here.
	Endpoint func(context.Context, container.InspectResponse, int) (string, error)
}

type Provider struct {
	cfg    Config
	engine Engine
	close  func() error
}

func New(cfg Config) (*Provider, error) {
	if cfg.DaemonPort == 0 {
		cfg.DaemonPort = 8080
	}
	if cfg.DaemonPort < 1 || cfg.DaemonPort > 65535 {
		return nil, fmt.Errorf("invalid daemon port")
	}
	if cfg.ReadyTimeout == 0 {
		cfg.ReadyTimeout = 2 * time.Minute
	}
	if cfg.ReadyTimeout < 0 {
		return nil, fmt.Errorf("negative readiness timeout")
	}
	if cfg.PublishAddress == "" {
		cfg.PublishAddress = "127.0.0.1"
	}
	if _, err := netip.ParseAddr(cfg.PublishAddress); err != nil {
		return nil, fmt.Errorf("invalid publish address: %w", err)
	}
	if cfg.DaemonHost == "" {
		cfg.DaemonHost = "127.0.0.1"
	}
	profiles, err := daemonprovider.Clone(cfg.Profiles)
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
	return p, nil
}

// Close closes only the internally constructed Engine client. Injected clients
// and the daemon HTTP transport remain owned by their caller.
func (p *Provider) Close() error {
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
	profile, err := daemonprovider.Clone(profile)
	if err != nil {
		return client.ContainerCreateOptions{}, profile, err
	}
	options := client.ContainerCreateOptions{Config: &profile.Config, HostConfig: &profile.HostConfig, NetworkingConfig: &profile.NetworkingConfig}
	if options.Config.WorkingDir == "" {
		options.Config.WorkingDir = "/workspace"
	}
	if len(options.Config.Entrypoint) == 0 {
		options.Config.Entrypoint = []string{"sandbox-daemon"}
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
	env["SANDBOX_ROOT"], env["SANDBOX_PORT"] = options.Config.WorkingDir, strconv.Itoa(p.cfg.DaemonPort)
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
	options.Name = "hastekit-" + sandbox.ResourceID(req.Session)
	port := network.MustParsePort(strconv.Itoa(p.cfg.DaemonPort) + "/tcp")
	if options.Config.ExposedPorts == nil {
		options.Config.ExposedPorts = network.PortSet{}
	}
	options.Config.ExposedPorts[port] = struct{}{}
	if p.cfg.Endpoint == nil {
		if options.HostConfig.NetworkMode == "host" || options.HostConfig.NetworkMode == "none" {
			return options, profile, fmt.Errorf("host/none networking requires an Endpoint callback")
		}
		if options.HostConfig.PortBindings == nil {
			options.HostConfig.PortBindings = network.PortMap{}
		}
		if len(options.HostConfig.PortBindings[port]) == 0 {
			options.HostConfig.PortBindings[port] = []network.PortBinding{{HostIP: netip.MustParseAddr(p.cfg.PublishAddress)}}
		}
	}
	if options.Config.Labels == nil {
		options.Config.Labels = map[string]string{}
	}
	delete(options.Config.Labels, daemonprovider.FingerprintKey)
	options.Config.Labels[daemonprovider.ManagedKey] = daemonprovider.ManagedValue
	fingerprint, err := daemonprovider.Fingerprint(req, options)
	if err != nil {
		return options, profile, err
	}
	options.Config.Labels[daemonprovider.FingerprintKey] = fingerprint
	return options, profile, nil
}

func (p *Provider) Create(ctx context.Context, req sandbox.CreateRequest) (sandbox.Sandbox, error) {
	if err := daemonprovider.Validate(req); err != nil {
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
		if config == nil || config.Labels[daemonprovider.ManagedKey] != daemonprovider.ManagedValue || config.Labels[daemonprovider.FingerprintKey] != options.Config.Labels[daemonprovider.FingerprintKey] {
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
		if info.ID != ref.ID || info.Config == nil || info.Config.Labels[daemonprovider.ManagedKey] != daemonprovider.ManagedValue {
			return nil, fmt.Errorf("%w: container identity or ownership mismatch", sandbox.ErrConflict)
		}
		if info.State == nil {
			return nil, fmt.Errorf("docker returned no container state")
		}
		if info.State.Dead || info.State.Paused || info.State.Status == "removing" {
			return nil, fmt.Errorf("%w: container is %s", sandbox.ErrConflict, info.State.Status)
		}
		if info.State.Running && !info.State.Restarting {
			endpoint, err := p.endpoint(ctx, info, p.cfg.DaemonPort)
			if err != nil {
				return nil, err
			}
			daemon, err := sandbox.NewDaemonSandbox(sandbox.DaemonConfig{Reference: ref, Endpoint: endpoint, Client: p.cfg.HTTPClient})
			if err != nil {
				return nil, err
			}
			if err := daemon.WaitReady(ctx); err != nil {
				return nil, err
			}
			return &runtime{DaemonSandbox: daemon, provider: p}, nil
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
			return "http://" + net.JoinHostPort(p.cfg.DaemonHost, bindings[0].HostPort), nil
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
	*sandbox.DaemonSandbox
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
