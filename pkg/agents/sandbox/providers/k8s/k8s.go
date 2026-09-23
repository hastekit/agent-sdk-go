// Package k8s provisions daemon-backed sandbox pods with the official Kubernetes client.
package k8s

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox/internal/daemonprovider"
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
// deleted implicitly. The daemon container defaults to the one named "sandbox".
type Profile struct {
	Template        corev1.PodTemplateSpec
	DaemonContainer string
}

type Config struct {
	Client       coreclient.CoreV1Interface
	RESTConfig   *rest.Config // used when Client is nil
	Kubeconfig   string       // used when Client/RESTConfig are nil; otherwise in-cluster then default kubeconfig loading
	Namespace    string       // defaults to "default"; must already exist
	Profiles     map[string]Profile
	DaemonPort   int           // defaults to 8080
	ReadyTimeout time.Duration // defaults to two minutes
	HTTPClient   *http.Client  // daemon transport; separate from API-server credentials
	// Configure runs on a deep copy of the template. It can select a session PVC
	// or subpath, mount uploads, or set scheduling. It must be stable for reuse.
	// Pod identity and ownership metadata are assigned after it returns.
	Configure func(context.Context, sandbox.CreateRequest, *corev1.Pod) error
	// Endpoint defaults to http://<pod IP>:<port>. Supply a resolver/proxy URL
	// when the SDK cannot route to pod IPs. The provider does not spawn kubectl.
	Endpoint func(context.Context, *corev1.Pod, int) (string, error)
}

type Provider struct {
	cfg    Config
	client coreclient.CoreV1Interface
}

func New(cfg Config) (*Provider, error) {
	if cfg.Namespace == "" {
		cfg.Namespace = "default"
	}
	if problems := validation.IsDNS1123Label(cfg.Namespace); len(problems) > 0 {
		return nil, fmt.Errorf("invalid Kubernetes namespace: %s", strings.Join(problems, ", "))
	}
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
	profiles := make(map[string]Profile, len(cfg.Profiles))
	for name, profile := range cfg.Profiles {
		profile.Template = *profile.Template.DeepCopy()
		if profile.DaemonContainer == "" {
			profile.DaemonContainer = "sandbox"
		}
		found := false
		for _, c := range profile.Template.Spec.Containers {
			if c.Name == profile.DaemonContainer && c.Image != "" {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("k8s profile %q requires daemon container %q with an image", name, profile.DaemonContainer)
		}
		profiles[name] = profile
	}
	cfg.Profiles = profiles
	if cfg.Client == nil {
		config := cfg.RESTConfig
		if config == nil {
			var err error
			if cfg.Kubeconfig == "" {
				config, err = rest.InClusterConfig()
			}
			if config == nil {
				if err != nil && !errors.Is(err, rest.ErrNotInCluster) {
					return nil, err
				}
				rules := clientcmd.NewDefaultClientConfigLoadingRules()
				rules.ExplicitPath = cfg.Kubeconfig
				config, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
				if err != nil {
					return nil, err
				}
			}
		}
		var err error
		cfg.Client, err = coreclient.NewForConfig(config)
		if err != nil {
			return nil, err
		}
	}
	return &Provider{cfg: cfg, client: cfg.Client}, nil
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
		if c.Name != profile.DaemonContainer {
			continue
		}
		if c.WorkingDir == "" {
			c.WorkingDir = "/workspace"
		}
		if len(c.Command) == 0 {
			c.Command = []string{"sandbox-daemon"}
		}
		// Replace explicit entries, including ValueFrom, when request env overrides.
		// Preserve template order: Kubernetes env values can reference earlier entries.
		for _, key := range slices.Sorted(maps.Keys(req.Env)) {
			setEnv(c, key, req.Env[key])
		}
		setEnv(c, "SANDBOX_ROOT", c.WorkingDir)
		setEnv(c, "SANDBOX_PORT", strconv.Itoa(p.cfg.DaemonPort))
	}
	if pod.Spec.RestartPolicy == "" {
		pod.Spec.RestartPolicy = corev1.RestartPolicyNever
	}
	if p.cfg.Configure != nil {
		if err := p.cfg.Configure(ctx, req, pod); err != nil {
			return nil, err
		}
	}
	found := false
	for _, c := range pod.Spec.Containers {
		if c.Name == profile.DaemonContainer && c.Image != "" {
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("configured pod has no daemon container")
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
	pod.Labels[daemonprovider.ManagedKey] = daemonprovider.ManagedValue
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	delete(pod.Annotations, daemonprovider.FingerprintKey)
	fingerprint, err := daemonprovider.Fingerprint(req, pod)
	if err != nil {
		return nil, err
	}
	pod.Annotations[daemonprovider.FingerprintKey] = fingerprint
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
	if err := daemonprovider.Validate(req); err != nil {
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
		if existing.Labels[daemonprovider.ManagedKey] != daemonprovider.ManagedValue || existing.Annotations[daemonprovider.FingerprintKey] != pod.Annotations[daemonprovider.FingerprintKey] {
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
	result, err := p.Connect(ctx, ref)
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
		if pod.Labels[daemonprovider.ManagedKey] != daemonprovider.ManagedValue {
			return nil, fmt.Errorf("%w: pod ownership mismatch", sandbox.ErrConflict)
		}
		if pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			return nil, fmt.Errorf("%w: pod is terminating or %s", sandbox.ErrConflict, pod.Status.Phase)
		}
		if pod.Status.Phase == corev1.PodRunning && pod.Status.PodIP != "" {
			endpoint, err := p.endpoint(ctx, pod, p.cfg.DaemonPort)
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
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
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
	*sandbox.DaemonSandbox
	provider *Provider
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
