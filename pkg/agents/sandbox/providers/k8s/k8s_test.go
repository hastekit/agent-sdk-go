package k8s

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox/internal/provision"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kuberuntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
)

// fakeStream stands in for the API server's exec subresource. It records what each
// stream was asked to do and answers the way the fixture's test wants.
type fakeStream struct {
	mu      sync.Mutex
	targets []*url.URL
	stdins  []string
	respond func(script string, target *url.URL, options remotecommand.StreamOptions) error
}

func (f *fakeStream) stream(ctx context.Context, target *url.URL, options remotecommand.StreamOptions) error {
	// A script ends with its block, and stdin stays open after that.
	var script strings.Builder
	lines := bufio.NewReader(options.Stdin)
	for {
		line, err := lines.ReadString('\n')
		script.WriteString(line)
		if err != nil || strings.HasSuffix(script.String(), "exit $?\n}\n") {
			break
		}
	}
	f.mu.Lock()
	f.targets = append(f.targets, target)
	f.stdins = append(f.stdins, script.String())
	respond := f.respond
	f.mu.Unlock()
	if respond != nil {
		return respond(script.String(), target, options)
	}
	return nil
}

func (f *fakeStream) last() (*url.URL, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.targets[len(f.targets)-1], f.stdins[len(f.stdins)-1]
}

func (f *fakeStream) setRespond(respond func(string, *url.URL, remotecommand.StreamOptions) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.respond = respond
}

func request() sandbox.CreateRequest {
	return sandbox.CreateRequest{Profile: "base", Session: sandbox.SessionKey{Namespace: "tenant", SessionID: "session"}, Env: map[string]string{"TOKEN": "secret", "A": "one", "Z": "two"}}
}
func fixture(t *testing.T, phase corev1.PodPhase, configure func(*Config)) (*Provider, *fake.Clientset) {
	p, client, _ := streamFixture(t, phase, configure)
	return p, client
}

func streamFixture(t *testing.T, phase corev1.PodPhase, configure func(*Config)) (*Provider, *fake.Clientset, *fakeStream) {
	t.Helper()
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "pods", func(action ktesting.Action) (bool, kuberuntime.Object, error) {
		pod := action.(ktesting.CreateAction).GetObject().(*corev1.Pod)
		pod.UID = types.UID("original-uid")
		pod.Status = corev1.PodStatus{Phase: phase, PodIP: "127.0.0.1"}
		if phase == corev1.PodRunning {
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "sandbox", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
		}
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
	cfg := Config{Client: client.CoreV1(), RESTConfig: &rest.Config{Host: "https://cluster.test"}, Namespace: "sandboxes", ReadyTimeout: time.Second, Profiles: map[string]Profile{"base": {Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "sandbox", Image: "sandbox-image", Env: []corev1.EnvVar{{Name: "PREFIX", Value: "base"}, {Name: "EXPANDED", Value: "$(PREFIX)/child"}}}}}}}}}
	if configure != nil {
		configure(&cfg)
	}
	provider, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { provider.Close() })
	streams := &fakeStream{}
	provider.stream = streams.stream
	return provider, client, streams
}

func TestLifecycleMountsConflictsAndImmutableReference(t *testing.T) {
	p, client, streams := streamFixture(t, corev1.PodRunning, func(cfg *Config) {
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
	streams.setRespond(func(_ string, _ *url.URL, options remotecommand.StreamOptions) error {
		io.WriteString(options.Stdout, "from the pod")
		return nil
	})
	result, err := sb.Exec(ctx, sandbox.ExecRequest{Argv: []string{"true"}})
	if err != nil || result.Stdout != "from the pod" {
		t.Fatal(result, err)
	}
	streams.setRespond(nil)
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
	if endpointCalls != 0 {
		t.Fatal("commands should not need a route to the pod")
	}
	if endpoint, err := sb.(sandbox.EndpointResolver).Endpoint(context.Background(), 9000); err != nil || endpoint != "http://127.0.0.1:9000" || endpointCalls != 1 {
		t.Fatalf("custom endpoint not used: %q %v", endpoint, err)
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
		if next.Annotations[provision.FingerprintKey] != first.Annotations[provision.FingerprintKey] {
			t.Fatal("unstable fingerprint")
		}
	}
	if len(p.cfg.Profiles["base"].Template.Spec.Containers[0].Env) != 2 {
		t.Fatal("template mutated")
	}
}

func TestPodHasNoDaemonAndABoundedLifetime(t *testing.T) {
	p, client := fixture(t, corev1.PodRunning, func(cfg *Config) { cfg.MaxLifetime = 90 * time.Minute })
	if _, err := p.Create(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	pods, _ := client.CoreV1().Pods("sandboxes").List(context.Background(), metav1.ListOptions{})
	pod := pods.Items[0]
	c := pod.Spec.Containers[0]
	if len(c.Command) != 3 || c.Command[0] != "sh" || c.Command[2] != keepalive || len(c.Ports) != 0 {
		t.Fatalf("container %+v", c)
	}
	if pod.Spec.ActiveDeadlineSeconds == nil || *pod.Spec.ActiveDeadlineSeconds != 5400 {
		t.Fatalf("deadline %v", pod.Spec.ActiveDeadlineSeconds)
	}
	for _, env := range c.Env {
		if env.Name == "SANDBOX_PORT" {
			t.Fatal(env)
		}
	}
	if pod.Annotations[containerKey] != "sandbox" || pod.Annotations[activityKey] == "" {
		t.Fatalf("annotations %v", pod.Annotations)
	}
}

func TestTemplateKeepsItsOwnCommandAndDeadline(t *testing.T) {
	deadline := int64(60)
	p, client := fixture(t, corev1.PodRunning, func(cfg *Config) {
		cfg.Profiles["custom"] = Profile{Container: "main", Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{ActiveDeadlineSeconds: &deadline, Containers: []corev1.Container{{Name: "main", Image: "sandbox-image", Command: []string{"my-init"}}}}}}
	})
	req := request()
	req.Profile = "custom"
	pod, err := p.pod(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if pod.Spec.Containers[0].Command[0] != "my-init" || *pod.Spec.ActiveDeadlineSeconds != 60 || pod.Annotations[containerKey] != "main" {
		t.Fatalf("%+v", pod)
	}
	_ = client
}

// Exec arguments in the URL end up in the API server's audit logs. A command can be
// a whole script, and its environment can hold secrets.
func TestCommandsTravelOverStdinAndNotInTheURL(t *testing.T) {
	p, _, streams := streamFixture(t, corev1.PodRunning, nil)
	ctx := context.Background()
	sb, err := p.Create(ctx, request())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sb.Exec(ctx, sandbox.ExecRequest{Argv: []string{"deploy", "--password=hunter2"}, Env: map[string]string{"API_KEY": "s3cret"}}); err != nil {
		t.Fatal(err)
	}
	target, script := streams.last()
	if strings.Contains(target.String(), "hunter2") || strings.Contains(target.String(), "s3cret") {
		t.Fatalf("secret in the URL: %s", target)
	}
	query := target.Query()
	if strings.Join(query["command"], " ") != "sh" || query.Get("container") != "sandbox" || query.Get("stdin") != "true" || query.Get("tty") == "true" {
		t.Fatalf("exec options: %v", query)
	}
	if !strings.HasPrefix(target.Path, "/api/v1/namespaces/sandboxes/pods/hastekit-") || !strings.HasSuffix(target.Path, "/exec") {
		t.Fatalf("path: %s", target.Path)
	}
	for _, want := range []string{"'--password=hunter2'", "'API_KEY=s3cret'", "set -- "} {
		if !strings.Contains(script, want) {
			t.Fatalf("%q not in stdin script:\n%s", want, script)
		}
	}
}

// File content is in a command's arguments, and those must stay out of the URL too.
func TestFileContentStaysOutOfTheURL(t *testing.T) {
	p, _, streams := streamFixture(t, corev1.PodRunning, nil)
	ctx := context.Background()
	sb, err := p.Create(ctx, request())
	if err != nil {
		t.Fatal(err)
	}
	if err := sandbox.WriteFile(ctx, sb, "secrets/report.txt", []byte("confidential")); err != nil {
		t.Fatal(err)
	}
	target, script := streams.last()
	if strings.Contains(target.String(), "report") || strings.Join(target.Query()["command"], " ") != "sh" {
		t.Fatalf("exposed in the URL: %s", target)
	}
	if !strings.Contains(script, "secrets/report.txt") {
		t.Fatalf("script:\n%s", script)
	}
}

const startupScript = "start-my-service"

func withStartupScript(cfg *Config) {
	profile := cfg.Profiles["base"]
	profile.StartupScript = startupScript
	cfg.Profiles["base"] = profile
}

// A pod starts once, so only creating one runs the script.
func TestStartupScriptRunsWhenAPodIsCreated(t *testing.T) {
	p, _, streams := streamFixture(t, corev1.PodRunning, withStartupScript)
	ctx := context.Background()
	ran := 0
	streams.setRespond(func(script string, _ *url.URL, _ remotecommand.StreamOptions) error {
		if strings.Contains(script, startupScript) {
			ran++
		}
		return nil
	})
	sb, err := p.Create(ctx, request())
	if err != nil {
		t.Fatal(err)
	}
	if ran != 1 {
		t.Fatal("ran", ran)
	}
	if _, err := p.Connect(ctx, sb.Reference()); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Create(ctx, request()); err != nil {
		t.Fatal(err)
	}
	if ran != 1 {
		t.Fatal("ran again for an existing pod", ran)
	}
}

func TestNoStartupScriptRunsNothing(t *testing.T) {
	p, client, streams := streamFixture(t, corev1.PodRunning, nil)
	if _, err := p.Create(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	if len(streams.targets) != 0 {
		t.Fatalf("ran %d commands", len(streams.targets))
	}
	pods, _ := client.CoreV1().Pods("sandboxes").List(context.Background(), metav1.ListOptions{})
	if _, ok := pods.Items[0].Annotations[provision.StartupScriptKey]; ok {
		t.Fatal("empty script stored")
	}
}

func TestFailedStartupDeletesTheNewPod(t *testing.T) {
	p, client, streams := streamFixture(t, corev1.PodRunning, withStartupScript)
	streams.setRespond(func(_ string, _ *url.URL, options remotecommand.StreamOptions) error {
		io.WriteString(options.Stderr, "registry unreachable")
		return utilexec.CodeExitError{Err: errors.New("terminated"), Code: 3}
	})
	if _, err := p.Create(context.Background(), request()); err == nil || !strings.Contains(err.Error(), "registry unreachable") {
		t.Fatalf("err=%v", err)
	}
	pods, _ := client.CoreV1().Pods("sandboxes").List(context.Background(), metav1.ListOptions{})
	if len(pods.Items) != 0 {
		t.Fatal("failed pod left behind")
	}
}

// The script is part of what was asked for, so a different one is a different sandbox.
func TestChangedStartupScriptIsAConflict(t *testing.T) {
	p, _ := fixture(t, corev1.PodRunning, withStartupScript)
	ctx := context.Background()
	if _, err := p.Create(ctx, request()); err != nil {
		t.Fatal(err)
	}
	profile := p.cfg.Profiles["base"]
	profile.StartupScript = "something-else"
	p.cfg.Profiles["base"] = profile
	if _, err := p.Create(ctx, request()); !errors.Is(err, sandbox.ErrConflict) {
		t.Fatalf("err=%v", err)
	}
}

func TestExitCodeComesFromTheStream(t *testing.T) {
	p, _, streams := streamFixture(t, corev1.PodRunning, nil)
	ctx := context.Background()
	sb, err := p.Create(ctx, request())
	if err != nil {
		t.Fatal(err)
	}
	streams.setRespond(func(string, *url.URL, remotecommand.StreamOptions) error {
		return utilexec.CodeExitError{Err: errors.New("terminated"), Code: 7}
	})
	result, err := sb.Exec(ctx, sandbox.ExecRequest{Argv: []string{"false"}})
	if err != nil || result.ExitCode != 7 {
		t.Fatalf("%+v %v", result, err)
	}
	broken := errors.New("stream broke")
	streams.setRespond(func(string, *url.URL, remotecommand.StreamOptions) error { return broken })
	if _, err := sb.Exec(ctx, sandbox.ExecRequest{Argv: []string{"true"}}); !errors.Is(err, broken) {
		t.Fatalf("err=%v", err)
	}
}

func TestStdinStaysOpenUntilTheCommandEnds(t *testing.T) {
	p, _, streams := streamFixture(t, corev1.PodRunning, nil)
	ctx := context.Background()
	sb, err := p.Create(ctx, request())
	if err != nil {
		t.Fatal(err)
	}
	early := make(chan error, 1)
	streams.setRespond(func(_ string, _ *url.URL, options remotecommand.StreamOptions) error {
		// The fake has read the script, so a further read waits for the call to end.
		read := make(chan error, 1)
		go func() {
			_, err := options.Stdin.Read(make([]byte, 1))
			read <- err
		}()
		select {
		case err := <-read:
			early <- err
		case <-time.After(100 * time.Millisecond):
			early <- nil
		}
		return nil
	})
	if _, err := sb.Exec(ctx, sandbox.ExecRequest{Argv: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	if err := <-early; err != nil {
		t.Fatalf("stdin closed while the command ran: %v", err)
	}
}

func activityOf(t *testing.T, client *fake.Clientset, name string) time.Time {
	t.Helper()
	pod, err := client.CoreV1().Pods("sandboxes").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := time.Parse(time.RFC3339, pod.Annotations[activityKey])
	if err != nil {
		t.Fatal(err)
	}
	return recorded
}

func setActivity(t *testing.T, client *fake.Clientset, name string, at time.Time) {
	t.Helper()
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{activityKey: at.UTC().Format(time.RFC3339)}}})
	if _, err := client.CoreV1().Pods("sandboxes").Patch(context.Background(), name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
}

func patches(client *fake.Clientset) int {
	count := 0
	for _, action := range client.Actions() {
		if action.GetVerb() == "patch" {
			count++
		}
	}
	return count
}

func TestUsingAPodRecordsActivity(t *testing.T) {
	p, client := fixture(t, corev1.PodRunning, nil)
	ctx := context.Background()
	sb, err := p.Create(ctx, request())
	if err != nil {
		t.Fatal(err)
	}
	name, _, _ := p.identity(sb.Reference())
	stale := time.Now().Add(-30 * time.Minute)

	setActivity(t, client, name, stale)
	again, err := p.Connect(ctx, sb.Reference())
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(activityOf(t, client, name)) > time.Minute {
		t.Fatal("connecting did not record activity")
	}

	// A burst of calls must not write to the API server for each one.
	before := patches(client)
	for range 3 {
		if _, err := p.Connect(ctx, sb.Reference()); err != nil {
			t.Fatal(err)
		}
		if _, err := again.Exec(ctx, sandbox.ExecRequest{Argv: []string{"true"}}); err != nil {
			t.Fatal(err)
		}
	}
	if got := patches(client) - before; got != 0 {
		t.Fatalf("%d writes for activity that was already fresh", got)
	}

	// A sandbox held across many calls keeps its pod alive with no reconnect.
	again.(*runtime).runner.renewed = stale
	setActivity(t, client, name, stale)
	if _, err := again.Exec(ctx, sandbox.ExecRequest{Argv: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	if time.Since(activityOf(t, client, name)) > time.Minute {
		t.Fatal("running a command did not record activity")
	}

	setActivity(t, client, name, stale)
	if err := again.(*runtime).KeepAlive(ctx); err != nil {
		t.Fatal(err)
	}
	if time.Since(activityOf(t, client, name)) > time.Minute {
		t.Fatal("KeepAlive did not record activity")
	}
}

// A command longer than the idle timeout must not look idle while it runs.
func TestLongOperationKeepsRecordingActivity(t *testing.T) {
	p, client, streams := streamFixture(t, corev1.PodRunning, func(cfg *Config) { cfg.IdleTimeout = time.Millisecond })
	p.Close()
	ctx := context.Background()
	sb, err := p.Create(ctx, request())
	if err != nil {
		t.Fatal(err)
	}
	before := patches(client)
	streams.setRespond(func(string, *url.URL, remotecommand.StreamOptions) error {
		time.Sleep(2500 * time.Millisecond)
		return nil
	})
	if _, err := sb.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sleep", "2"}}); err != nil {
		t.Fatal(err)
	}
	if got := patches(client) - before; got < 2 {
		t.Fatalf("recorded activity %d times during the command", got)
	}
}

func TestMissingPatchPermissionFailsLoudly(t *testing.T) {
	p, client := fixture(t, corev1.PodRunning, nil)
	ctx := context.Background()
	sb, err := p.Create(ctx, request())
	if err != nil {
		t.Fatal(err)
	}
	name, _, _ := p.identity(sb.Reference())
	setActivity(t, client, name, time.Now().Add(-time.Hour))
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, name, errors.New("denied"))
	client.PrependReactor("patch", "pods", func(ktesting.Action) (bool, kuberuntime.Object, error) { return true, nil, forbidden })
	if _, err := p.Connect(ctx, sb.Reference()); !apierrors.IsForbidden(err) {
		t.Fatalf("err=%v", err)
	}
}

func seedPod(t *testing.T, client *fake.Clientset, name string, labels, annotations map[string]string, phase corev1.PodPhase, created time.Time) {
	t.Helper()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "sandboxes", UID: types.UID(name), ResourceVersion: "1", Labels: labels, Annotations: annotations, CreationTimestamp: metav1.NewTime(created)}, Status: corev1.PodStatus{Phase: phase}}
	if err := client.Tracker().Add(pod); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteIdleDeletesOnlyWhatIsIdle(t *testing.T) {
	p, client := fixture(t, corev1.PodRunning, func(cfg *Config) { cfg.IdleTimeout = -1 })
	if p.stopSweep != nil {
		t.Fatal("started a sweep")
	}
	managed := map[string]string{provision.ManagedKey: provision.ManagedValue}
	old, recent := time.Now().Add(-2*time.Hour), time.Now().Add(-time.Minute)
	stamp := func(at time.Time) map[string]string {
		return map[string]string{activityKey: at.UTC().Format(time.RFC3339)}
	}
	seedPod(t, client, "idle", managed, stamp(old), corev1.PodRunning, old)
	seedPod(t, client, "used", managed, stamp(recent), corev1.PodRunning, old)
	seedPod(t, client, "just-created", managed, nil, corev1.PodPending, recent)
	seedPod(t, client, "never-recorded", managed, nil, corev1.PodRunning, old)
	seedPod(t, client, "stuck-pending", managed, stamp(old), corev1.PodPending, old)
	seedPod(t, client, "finished", managed, stamp(old), corev1.PodSucceeded, old)
	seedPod(t, client, "not-ours", nil, stamp(old), corev1.PodRunning, old)
	deleted, err := p.DeleteIdle(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 4 {
		t.Fatalf("deleted %d", deleted)
	}
	pods, _ := client.CoreV1().Pods("sandboxes").List(context.Background(), metav1.ListOptions{})
	left := map[string]bool{}
	for _, pod := range pods.Items {
		left[pod.Name] = true
	}
	if len(left) != 3 || !left["used"] || !left["just-created"] || !left["not-ours"] {
		t.Fatalf("left %v", left)
	}
	if _, err := p.DeleteIdle(context.Background(), -time.Second); err == nil {
		t.Fatal("accepted a negative idle duration")
	}
}

// A pod used between listing and deleting has a new resource version, and the API
// server refuses the delete. That is not a failure, and the pod stays.
func TestDeleteIdleLosesToAPodBeingUsed(t *testing.T) {
	p, client := fixture(t, corev1.PodRunning, func(cfg *Config) { cfg.IdleTimeout = -1 })
	old := time.Now().Add(-2 * time.Hour)
	seedPod(t, client, "idle", map[string]string{provision.ManagedKey: provision.ManagedValue}, nil, corev1.PodRunning, old)
	var version string
	client.PrependReactor("delete", "pods", func(action ktesting.Action) (bool, kuberuntime.Object, error) {
		options := action.(ktesting.DeleteAction).GetDeleteOptions()
		version = *options.Preconditions.ResourceVersion
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, "idle", errors.New("modified"))
	})
	deleted, err := p.DeleteIdle(context.Background(), time.Hour)
	if err != nil || deleted != 0 || version != "1" {
		t.Fatalf("deleted=%d err=%v version=%q", deleted, err, version)
	}
}

func TestProviderSweepsOnItsOwn(t *testing.T) {
	p, client := fixture(t, corev1.PodRunning, func(cfg *Config) { cfg.IdleTimeout = time.Millisecond })
	old := time.Now().Add(-time.Hour)
	seedPod(t, client, "idle", map[string]string{provision.ManagedKey: provision.ManagedValue}, nil, corev1.PodRunning, old)
	deadline := time.Now().Add(5 * time.Second)
	for {
		pods, _ := client.CoreV1().Pods("sandboxes").List(context.Background(), metav1.ListOptions{})
		if len(pods.Items) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("nothing deleted the idle pod")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestConfigValidation(t *testing.T) {
	if _, err := New(Config{Client: fake.NewSimpleClientset().CoreV1()}); err == nil {
		t.Fatal("accepted a client that cannot run commands")
	}
	if _, err := New(Config{RESTConfig: &rest.Config{Host: "https://cluster.test"}, MaxLifetime: -time.Second}); err == nil {
		t.Fatal("accepted a negative max lifetime")
	}
}

// A pod whose container has stopped, with a sidecar still running, is not waiting
// to become ready.
func TestConnectRejectsAnExitedContainer(t *testing.T) {
	p, client := fixture(t, corev1.PodRunning, nil)
	ctx := context.Background()
	sb, err := p.Create(ctx, request())
	if err != nil {
		t.Fatal(err)
	}
	name, _, _ := p.identity(sb.Reference())
	pod, _ := client.CoreV1().Pods("sandboxes").Get(ctx, name, metav1.GetOptions{})
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}
	if _, err := client.CoreV1().Pods("sandboxes").Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Connect(ctx, sb.Reference()); !errors.Is(err, sandbox.ErrConflict) {
		t.Fatal(fmt.Sprint("err=", err))
	}
}

// Exec does not care what a pod's entrypoint is, so a pod created with the daemon
// keeps working, and it says less about itself than a new one does.
func TestPodFromBeforeExecStillConnects(t *testing.T) {
	p, client, streams := streamFixture(t, corev1.PodRunning, nil)
	ctx := context.Background()
	old := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "sandboxes", UID: "old-uid", Labels: map[string]string{provision.ManagedKey: provision.ManagedValue}, CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour))},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "sandbox", Image: "old-image", Command: []string{"sandbox-daemon"}, WorkingDir: "/data"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "sandbox", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}},
	}
	if err := client.Tracker().Add(old); err != nil {
		t.Fatal(err)
	}
	ref, _ := reference(old)
	sb, err := p.Connect(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sb.Exec(ctx, sandbox.ExecRequest{Argv: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	target, script := streams.last()
	if target.Query().Get("container") != "sandbox" || !strings.Contains(script, "'/data'") {
		t.Fatalf("container %q, script:\n%s", target.Query().Get("container"), script)
	}
	if time.Since(activityOf(t, client, "old")) > time.Minute {
		t.Fatal("connecting did not start recording activity")
	}
}
