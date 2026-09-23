package k8s

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox/internal/daemonprovider"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kuberuntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func request() sandbox.CreateRequest {
	return sandbox.CreateRequest{Profile: "base", Session: sandbox.SessionKey{Namespace: "tenant", SessionID: "session"}, Env: map[string]string{"TOKEN": "secret", "A": "one", "Z": "two"}}
}
func fixture(t *testing.T, phase corev1.PodPhase, configure func(*Config)) (*Provider, *fake.Clientset) {
	t.Helper()
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Write([]byte("ok"))
			return
		}
		if r.URL.Path == "/v2/exec" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"stdout":"pod daemon","exit_code":0}`))
			return
		}
		t.Errorf("unexpected daemon route %s", r.URL.Path)
		w.WriteHeader(404)
	}))
	t.Cleanup(daemon.Close)
	u, _ := url.Parse(daemon.URL)
	port, _ := strconv.Atoi(u.Port())
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "pods", func(action ktesting.Action) (bool, kuberuntime.Object, error) {
		pod := action.(ktesting.CreateAction).GetObject().(*corev1.Pod)
		pod.UID = types.UID("original-uid")
		pod.Status = corev1.PodStatus{Phase: phase, PodIP: "127.0.0.1"}
		return false, nil, nil
	})
	// The simple tracker does not implement UID preconditions. Emulate the API
	// server check so stale-reference deletion exercises the actual request.
	client.PrependReactor("delete", "pods", func(action ktesting.Action) (bool, kuberuntime.Object, error) {
		delete := action.(ktesting.DeleteAction)
		object, err := client.Tracker().Get(corev1.SchemeGroupVersion.WithResource("pods"), action.GetNamespace(), delete.GetName())
		if err != nil {
			return true, nil, err
		}
		pod := object.(*corev1.Pod)
		options := delete.GetDeleteOptions()
		if options.Preconditions == nil || options.Preconditions.UID == nil {
			t.Error("deletion omitted UID precondition")
			return true, nil, errors.New("missing UID")
		}
		if *options.Preconditions.UID != pod.UID {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, pod.Name, errors.New("UID differs"))
		}
		return false, nil, nil
	})
	cfg := Config{Client: client.CoreV1(), Namespace: "sandboxes", DaemonPort: port, ReadyTimeout: time.Second, Profiles: map[string]Profile{"base": {Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "sandbox", Image: "daemon-image", Env: []corev1.EnvVar{{Name: "PREFIX", Value: "base"}, {Name: "EXPANDED", Value: "$(PREFIX)/child"}}}}}}}}}
	if configure != nil {
		configure(&cfg)
	}
	provider, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return provider, client
}

func TestLifecycleMountsConflictsAndImmutableReference(t *testing.T) {
	p, client := fixture(t, corev1.PodRunning, func(cfg *Config) {
		cfg.Configure = func(_ context.Context, req sandbox.CreateRequest, pod *corev1.Pod) error {
			pod.Spec.Volumes = []corev1.Volume{{Name: "uploads", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "shared-uploads"}}}}
			pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "uploads", MountPath: "/workspace/uploads", SubPath: req.Session.SessionID}}
			return nil
		}
	})
	ctx := context.Background()
	req := request()
	sb, err := p.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	name, uid, err := p.identity(sb.Reference())
	if err != nil || uid != "original-uid" {
		t.Fatal(sb.Reference(), err)
	}
	pod, err := client.CoreV1().Pods("sandboxes").Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != "shared-uploads" || pod.Spec.Containers[0].VolumeMounts[0].SubPath != "session" {
		t.Fatal("application storage configuration lost")
	}
	if pod.Spec.Containers[0].Env[0].Name != "PREFIX" || pod.Spec.Containers[0].Env[1].Name != "EXPANDED" {
		t.Fatal("template env order changed")
	}
	result, err := sb.Exec(ctx, sandbox.ExecRequest{Argv: []string{"true"}})
	if err != nil || result.Stdout != "pod daemon" {
		t.Fatal(result, err)
	}
	reused, err := p.Create(ctx, req)
	if err != nil || reused.Reference() != sb.Reference() {
		t.Fatal("matching conflict not recovered", err)
	}
	req.Env["A"] = "changed"
	if _, err := p.Create(ctx, req); !errors.Is(err, sandbox.ErrConflict) {
		t.Fatalf("mismatched configuration: %v", err)
	}
	pods, _ := client.CoreV1().Pods("sandboxes").List(ctx, metav1.ListOptions{})
	if len(pods.Items) != 1 {
		t.Fatal("conflict deleted existing compute")
	}
	if _, err := p.Connect(ctx, sb.Reference()); err != nil {
		t.Fatal(err)
	}
	// Simulate a pod deleted and recreated at the same stable name.
	pod.UID = "replacement-uid"
	if _, err := client.CoreV1().Pods("sandboxes").Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Connect(ctx, sb.Reference()); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatal("adopted replacement pod", err)
	}
	if err := p.Delete(ctx, sb.Reference()); !errors.Is(err, sandbox.ErrConflict) {
		t.Fatal("deleted replacement pod", err)
	}
	ref, _ := reference(pod)
	if err := p.Delete(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if err := p.Delete(ctx, ref); err != nil {
		t.Fatal("delete not idempotent", err)
	}
	if _, err := p.Connect(ctx, ref); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatal(err)
	}
	for _, action := range client.Actions() {
		if action.GetResource().Resource == "persistentvolumeclaims" {
			t.Fatal("provider managed application PVC")
		}
	}
}

func TestReadinessFailureCleansOnlyOwnedPod(t *testing.T) {
	for _, phase := range []corev1.PodPhase{corev1.PodFailed, corev1.PodPending} {
		t.Run(string(phase), func(t *testing.T) {
			p, client := fixture(t, phase, func(cfg *Config) { cfg.ReadyTimeout = 30 * time.Millisecond })
			if _, err := p.Create(context.Background(), request()); err == nil {
				t.Fatal("expected readiness failure")
			}
			pods, _ := client.CoreV1().Pods("sandboxes").List(context.Background(), metav1.ListOptions{})
			if len(pods.Items) != 0 {
				t.Fatal("owned failed pod leaked")
			}
		})
	}
	p, client := fixture(t, corev1.PodRunning, nil)
	sb, err := p.Create(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	name, _, _ := p.identity(sb.Reference())
	pod, _ := client.CoreV1().Pods("sandboxes").Get(context.Background(), name, metav1.GetOptions{})
	pod.Status.Phase = corev1.PodFailed
	client.CoreV1().Pods("sandboxes").Update(context.Background(), pod, metav1.UpdateOptions{})
	if _, err := p.Create(context.Background(), request()); err == nil {
		t.Fatal("adopted failed pod")
	}
	if _, err := client.CoreV1().Pods("sandboxes").Get(context.Background(), name, metav1.GetOptions{}); err != nil {
		t.Fatal("deleted another creator's pod", err)
	}
}

func TestLookupErrorsAndCustomEndpoint(t *testing.T) {
	endpointCalls := 0
	p, client := fixture(t, corev1.PodRunning, func(cfg *Config) {
		cfg.Endpoint = func(_ context.Context, pod *corev1.Pod, port int) (string, error) {
			endpointCalls++
			return "http://" + pod.Status.PodIP + ":" + strconv.Itoa(port), nil
		}
	})
	sb, err := p.Create(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if endpointCalls != 1 {
		t.Fatal("custom endpoint not used")
	}
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "sandbox", errors.New("denied"))
	client.PrependReactor("get", "pods", func(ktesting.Action) (bool, kuberuntime.Object, error) { return true, nil, forbidden })
	if _, err := p.Connect(context.Background(), sb.Reference()); !apierrors.IsForbidden(err) {
		t.Fatal("lookup error hidden", err)
	}
	client.ClearActions()
	if _, err := p.Create(context.Background(), request()); !errors.Is(err, sandbox.ErrConflict) || !errors.Is(err, forbidden) {
		t.Fatal("conflict lookup error lost", err)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "delete" {
			t.Fatal("lookup failure deleted resource")
		}
	}
}

func TestProfileIsolationAndStableFingerprint(t *testing.T) {
	p, _ := fixture(t, corev1.PodRunning, nil)
	first, err := p.pod(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		next, err := p.pod(context.Background(), request())
		if err != nil {
			t.Fatal(err)
		}
		if next.Annotations[daemonprovider.FingerprintKey] != first.Annotations[daemonprovider.FingerprintKey] {
			t.Fatal("unstable fingerprint")
		}
	}
	if len(p.cfg.Profiles["base"].Template.Spec.Containers[0].Env) != 2 {
		t.Fatal("template mutated")
	}
}
