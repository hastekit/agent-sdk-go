// Package k8s provisions sandbox pods with the official Kubernetes client and runs
// commands in them through the API server's exec subresource. Nothing of ours runs
// inside the pod beyond a shell keeping it alive.
package k8s

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox/internal/execsandbox"
	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox/internal/provision"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Profile uses a native pod template for images, resource limits, PVCs, secrets,
// service accounts, scheduling, and security settings. No storage is created or
// deleted implicitly. Commands run in the container named "sandbox" unless
// Container says otherwise. The image must provide a POSIX shell and the tools
// listed in the README.
type Profile struct {
	Template  corev1.PodTemplateSpec
	Container string
	// StartupScript runs in sh when a pod has been created, as the last step of
	// Create. It is stored in an annotation, so keep secrets out of it. It shares
	// ReadyTimeout, and a pod whose script fails is deleted.
	StartupScript string
}

// keepalive holds the container open between commands, and is what commands
// without a parent end up attached to. A shell reaps those while it waits, and as
// PID 1 it gets no signal it has not asked for, so it asks for the stop signal.
const keepalive = `trap "exit 0" TERM INT
while :; do
	sleep 3600 &
	wait "$!"
done
`

const (
	// containerKey names the container commands run in, since a reference carries
	// no profile to look it up in.
	containerKey = "hastekit.container"

	// activityKey holds the time a pod was last used. It lives on the pod so that
	// every provider using this namespace sees the same answer.
	activityKey = "hastekit.last-activity"

	defaultContainer = "sandbox"
)

type Config struct {
	Client       coreclient.CoreV1Interface // for API calls; requires RESTConfig too, because exec cannot go through this interface
	RESTConfig   *rest.Config               // required with Client; otherwise loaded from Kubeconfig, in-cluster, or default kubeconfig loading
	Kubeconfig   string                     // used when RESTConfig is nil
	Namespace    string                     // defaults to "default"; must already exist
	Profiles     map[string]Profile
	ReadyTimeout time.Duration // includes scheduling, startup and the startup script; defaults to two minutes
	// IdleTimeout deletes a pod after this long without activity. It defaults to
	// one hour, and a negative value leaves deleting to the caller, through
	// DeleteIdle. Pods cannot be paused or stopped, so this loses everything not
	// on a volume, and the session's next sandbox call fails until its state is
	// cleared. Every provider using a namespace sweeps its pods, so the shortest
	// timeout among them wins.
	IdleTimeout time.Duration
	// MaxLifetime becomes the pod's activeDeadlineSeconds unless its template sets
	// one, which bounds its lifetime when no provider is left to notice it is
	// idle. Defaults to 24 hours.
	MaxLifetime time.Duration
	// Configure runs on a deep copy of the template. It can select a session PVC
	// or subpath, mount uploads, or set scheduling. It must be stable for reuse.
	// Pod identity and ownership metadata are assigned after it returns.
	Configure func(context.Context, sandbox.CreateRequest, *corev1.Pod) error
	// Endpoint resolves service ports requested through sandbox.EndpointResolver.
	// It defaults to http://<pod IP>:<port>. Supply a resolver/proxy URL when the
	// SDK cannot route to pod IPs.
	Endpoint func(context.Context, *corev1.Pod, int) (string, error)
}

type Provider struct {
	cfg        Config
	client     coreclient.CoreV1Interface
	execClient *coreclient.CoreV1Client
	stream     streamFunc
	stopSweep  context.CancelFunc
	sweepEnded chan struct{}
}

func New(cfg Config) (*Provider, error) {
	if cfg.Namespace == "" {
		cfg.Namespace = "default"
	}
	if problems := validation.IsDNS1123Label(cfg.Namespace); len(problems) > 0 {
		return nil, fmt.Errorf("invalid Kubernetes namespace: %s", strings.Join(problems, ", "))
	}
	if cfg.ReadyTimeout == 0 {
		cfg.ReadyTimeout = 2 * time.Minute
	}
	if cfg.ReadyTimeout < 0 {
		return nil, fmt.Errorf("negative readiness timeout")
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = time.Hour
	}
	if cfg.MaxLifetime == 0 {
		cfg.MaxLifetime = 24 * time.Hour
	}
	if cfg.MaxLifetime < 0 {
		return nil, fmt.Errorf("negative max lifetime")
	}
	profiles := make(map[string]Profile, len(cfg.Profiles))
	for name, profile := range cfg.Profiles {
		profile.Template = *profile.Template.DeepCopy()
		if profile.Container == "" {
			profile.Container = defaultContainer
		}
		found := false
		for _, c := range profile.Template.Spec.Containers {
			if c.Name == profile.Container && c.Image != "" {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("k8s profile %q requires container %q with an image", name, profile.Container)
		}
		profiles[name] = profile
	}
	cfg.Profiles = profiles
	if cfg.RESTConfig == nil {
		if cfg.Client != nil {
			return nil, fmt.Errorf("Config.Client also requires Config.RESTConfig, which running commands needs")
		}
		var err error
		if cfg.Kubeconfig == "" {
			cfg.RESTConfig, err = rest.InClusterConfig()
		}
		if cfg.RESTConfig == nil {
			if err != nil && !errors.Is(err, rest.ErrNotInCluster) {
				return nil, err
			}
			rules := clientcmd.NewDefaultClientConfigLoadingRules()
			rules.ExplicitPath = cfg.Kubeconfig
			cfg.RESTConfig, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
			if err != nil {
				return nil, err
			}
		}
	}
	execClient, err := coreclient.NewForConfig(cfg.RESTConfig)
	if err != nil {
		return nil, err
	}
	if cfg.Client == nil {
		cfg.Client = execClient
	}
	p := &Provider{cfg: cfg, client: cfg.Client, execClient: execClient}
	p.stream = p.openStream
	if cfg.IdleTimeout > 0 {
		var ctx context.Context
		ctx, p.stopSweep = context.WithCancel(context.Background())
		p.sweepEnded = make(chan struct{})
		go p.sweep(ctx)
	}
	return p, nil
}

// Close stops the idle sweep. Pods keep running, and Connect still reaches them.
func (p *Provider) Close() error {
	if p.stopSweep != nil {
		p.stopSweep()
		<-p.sweepEnded
	}
	return nil
}

func (p *Provider) pod(ctx context.Context, req sandbox.CreateRequest) (*corev1.Pod, error) {
	profile, ok := p.cfg.Profiles[req.Profile]
	if !ok {
		return nil, fmt.Errorf("unknown k8s profile %q", req.Profile)
	}
	template := profile.Template.DeepCopy()
	pod := &corev1.Pod{ObjectMeta: template.ObjectMeta, Spec: template.Spec}
	for index := range pod.Spec.Containers {
		c := &pod.Spec.Containers[index]
		if c.Name != profile.Container {
			continue
		}
		if c.WorkingDir == "" {
			c.WorkingDir = "/workspace"
		}
		if len(c.Command) == 0 {
			c.Command = []string{"sh", "-c", keepalive}
		}
		// Replace explicit entries, including ValueFrom, when request env overrides.
		// Preserve template order: Kubernetes env values can reference earlier entries.
		for _, key := range slices.Sorted(maps.Keys(req.Env)) {
			setEnv(c, key, req.Env[key])
		}
		setEnv(c, "SANDBOX_ROOT", c.WorkingDir)
	}
	if pod.Spec.RestartPolicy == "" {
		pod.Spec.RestartPolicy = corev1.RestartPolicyNever
	}
	if pod.Spec.ActiveDeadlineSeconds == nil {
		seconds := int64((p.cfg.MaxLifetime + time.Second - 1) / time.Second)
		pod.Spec.ActiveDeadlineSeconds = &seconds
	}
	if p.cfg.Configure != nil {
		if err := p.cfg.Configure(ctx, req, pod); err != nil {
			return nil, err
		}
	}
	found := false
	for _, c := range pod.Spec.Containers {
		if c.Name == profile.Container && c.Image != "" {
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("configured pod has no container %q", profile.Container)
	}
	pod.Name = "hastekit-" + sandbox.ResourceID(req.Session)
	pod.GenerateName = ""
	pod.Namespace = p.cfg.Namespace
	pod.UID = ""
	pod.ResourceVersion = ""
	pod.Status = corev1.PodStatus{}
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels[provision.ManagedKey] = provision.ManagedValue
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	delete(pod.Annotations, provision.FingerprintKey)
	delete(pod.Annotations, activityKey)
	delete(pod.Annotations, provision.StartupScriptKey)
	pod.Annotations[containerKey] = profile.Container
	if profile.StartupScript != "" {
		pod.Annotations[provision.StartupScriptKey] = profile.StartupScript
	}
	fingerprint, err := provision.Fingerprint(req, pod)
	if err != nil {
		return nil, err
	}
	pod.Annotations[provision.FingerprintKey] = fingerprint
	pod.Annotations[activityKey] = time.Now().UTC().Format(time.RFC3339)
	return pod, nil
}

func setEnv(c *corev1.Container, key, value string) {
	for i := range c.Env {
		if c.Env[i].Name == key {
			c.Env[i] = corev1.EnvVar{Name: key, Value: value}
			return
		}
	}
	c.Env = append(c.Env, corev1.EnvVar{Name: key, Value: value})
}

func (p *Provider) Create(ctx context.Context, req sandbox.CreateRequest) (sandbox.Sandbox, error) {
	if err := provision.Validate(req); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.ReadyTimeout)
	defer cancel()
	pod, err := p.pod(ctx, req)
	if err != nil {
		return nil, err
	}
	created, err := p.client.Pods(p.cfg.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, lookupErr := p.client.Pods(p.cfg.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if lookupErr != nil {
			return nil, errors.Join(mapError(err), mapError(lookupErr))
		}
		if existing.Labels[provision.ManagedKey] != provision.ManagedValue || existing.Annotations[provision.FingerprintKey] != pod.Annotations[provision.FingerprintKey] {
			return nil, fmt.Errorf("%w: named pod has different configuration", sandbox.ErrConflict)
		}
		ref, err := reference(existing)
		if err != nil {
			return nil, err
		}
		return p.Connect(ctx, ref)
	}
	if err != nil {
		return nil, mapError(err)
	}
	ref, err := reference(created)
	if err != nil {
		return nil, err
	}
	result, err := p.connect(ctx, ref, true)
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		err = errors.Join(err, p.Delete(cleanup, ref))
	}
	return result, err
}

// Include namespace, name AND immutable UID. A replacement at the same name is
// a different sandbox and must not be adopted by a stale run.State reference.
func reference(pod *corev1.Pod) (sandbox.Reference, error) {
	if pod.UID == "" {
		return sandbox.Reference{}, fmt.Errorf("Kubernetes returned a pod without a UID")
	}
	return sandbox.Reference{Provider: "k8s", ID: pod.Namespace + ":" + pod.Name + ":" + string(pod.UID)}, nil
}
func (p *Provider) identity(ref sandbox.Reference) (string, types.UID, error) {
	parts := strings.Split(ref.ID, ":")
	if ref.Provider != "k8s" || len(parts) != 3 || parts[0] != p.cfg.Namespace || len(validation.IsDNS1123Subdomain(parts[1])) > 0 || parts[2] == "" {
		return "", "", fmt.Errorf("invalid k8s reference or namespace")
	}
	return parts[1], types.UID(parts[2]), nil
}
func mapError(err error) error {
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("%w: %w", sandbox.ErrNotFound, err)
	}
	if apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err) {
		return fmt.Errorf("%w: %w", sandbox.ErrConflict, err)
	}
	return err
}

func (p *Provider) Connect(ctx context.Context, ref sandbox.Reference) (sandbox.Sandbox, error) {
	return p.connect(ctx, ref, false)
}

// connect waits for the pod's container to run. A pod has one start, so a pod this
// call has just created is the only one with a startup script still to run.
func (p *Provider) connect(ctx context.Context, ref sandbox.Reference, created bool) (sandbox.Sandbox, error) {
	name, uid, err := p.identity(ref)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.ReadyTimeout)
	defer cancel()
	for {
		pod, err := p.client.Pods(p.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, mapError(err)
		}
		if pod.UID != uid {
			return nil, fmt.Errorf("%w: original pod was replaced", sandbox.ErrNotFound)
		}
		if pod.Labels[provision.ManagedKey] != provision.ManagedValue {
			return nil, fmt.Errorf("%w: pod ownership mismatch", sandbox.ErrConflict)
		}
		if pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			return nil, fmt.Errorf("%w: pod is terminating or %s", sandbox.ErrConflict, pod.Status.Phase)
		}
		container := pod.Annotations[containerKey]
		if container == "" {
			container = defaultContainer
		}
		state := containerState(pod, container)
		if state != nil && state.Terminated != nil {
			return nil, fmt.Errorf("%w: container %s has exited", sandbox.ErrConflict, container)
		}
		if pod.Status.Phase == corev1.PodRunning && state != nil && state.Running != nil {
			return p.runtime(ctx, ref, pod, container, created)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
func containerState(pod *corev1.Pod, name string) *corev1.ContainerState {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == name {
			return &pod.Status.ContainerStatuses[i].State
		}
	}
	return nil
}

func (p *Provider) runtime(ctx context.Context, ref sandbox.Reference, pod *corev1.Pod, container string, created bool) (sandbox.Sandbox, error) {
	r := &runner{provider: p, name: pod.Name, uid: pod.UID, container: container, renewed: lastActivity(pod)}
	if err := r.renewIfDue(ctx); err != nil {
		return nil, err
	}
	root := "/workspace"
	for _, c := range pod.Spec.Containers {
		if c.Name == container && c.WorkingDir != "" {
			root = c.WorkingDir
		}
	}
	result := &runtime{Sandbox: execsandbox.New(execsandbox.Config{Reference: ref, Runner: r, Root: root}), provider: p, runner: r}
	if created {
		if err := result.Startup(ctx, pod.Annotations[provision.StartupScriptKey], p.cfg.ReadyTimeout); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (p *Provider) endpoint(ctx context.Context, pod *corev1.Pod, port int) (string, error) {
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("invalid service port")
	}
	if p.cfg.Endpoint != nil {
		return p.cfg.Endpoint(ctx, pod, port)
	}
	if pod.Status.PodIP == "" {
		return "", fmt.Errorf("pod has no IP")
	}
	return "http://" + net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(port)), nil
}
func (p *Provider) Delete(ctx context.Context, ref sandbox.Reference) error {
	name, uid, err := p.identity(ref)
	if err != nil {
		return err
	}
	policy := metav1.DeletePropagationBackground
	err = p.client.Pods(p.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}, PropagationPolicy: &policy})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return mapError(err)
}

type runtime struct {
	*execsandbox.Sandbox
	provider *Provider
	runner   *runner
}

// KeepAlive counts as activity, for when the pod is busy with something that does
// not go through exec, like traffic to a service running inside it.
func (s *runtime) KeepAlive(ctx context.Context) error {
	return s.runner.renew(ctx)
}

func (s *runtime) Endpoint(ctx context.Context, port int) (string, error) {
	name, uid, err := s.provider.identity(s.Reference())
	if err != nil {
		return "", err
	}
	pod, err := s.provider.client.Pods(s.provider.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", mapError(err)
	}
	if pod.UID != uid {
		return "", fmt.Errorf("%w: original pod was replaced", sandbox.ErrNotFound)
	}
	return s.provider.endpoint(ctx, pod, port)
}

var _ sandbox.Provider = (*Provider)(nil)
