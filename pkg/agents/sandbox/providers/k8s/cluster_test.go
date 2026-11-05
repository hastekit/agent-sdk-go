package k8s

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox/internal/sandboxtest"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/clientcmd"
)

// TestRealCluster runs what the fake can only imitate against a Kubernetes cluster,
// once for each image in SANDBOX_K8S_TEST_IMAGES, a comma-separated list. It creates
// and deletes pods and a namespace, so it only ever uses the kubeconfig context
// named in SANDBOX_K8S_TEST_CONTEXT, never the current one. Point that at a
// disposable cluster:
//
//	SANDBOX_K8S_TEST_CONTEXT=kind-test SANDBOX_K8S_TEST_IMAGES=alpine:3.20,debian:bookworm-slim go test ./pkg/agents/sandbox/providers/k8s/
func TestRealCluster(t *testing.T) {
	kubeContext, images := os.Getenv("SANDBOX_K8S_TEST_CONTEXT"), os.Getenv("SANDBOX_K8S_TEST_IMAGES")
	if kubeContext == "" || images == "" {
		t.Skip("SANDBOX_K8S_TEST_CONTEXT and SANDBOX_K8S_TEST_IMAGES are not both set")
	}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{CurrentContext: kubeContext}).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	client, err := coreclient.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	namespace := "hastekit-sdk-test-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := client.Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Namespaces().Delete(context.Background(), namespace, metav1.DeleteOptions{}); err != nil {
			t.Error(err)
		}
	})
	for _, image := range strings.Split(images, ",") {
		image = strings.TrimSpace(image)
		t.Run(image, func(t *testing.T) {
			template := corev1.PodTemplateSpec{Spec: corev1.PodSpec{TerminationGracePeriodSeconds: new(int64), Containers: []corev1.Container{{Name: "sandbox", Image: image, ImagePullPolicy: corev1.PullIfNotPresent}}}}
			p, err := New(Config{RESTConfig: config, Namespace: namespace, IdleTimeout: -1, ReadyTimeout: time.Minute, Profiles: map[string]Profile{
				"base":   {Template: template, StartupScript: "sleep 4441 >/dev/null 2>&1 &\necho started >>/tmp/starts\n"},
				"broken": {Template: template, StartupScript: "echo cannot start >&2; exit 3"},
			}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { p.Close() })
			req := sandbox.CreateRequest{Profile: "base", Session: sandbox.SessionKey{Namespace: "sdk-test", SessionID: strconv.FormatInt(time.Now().UnixNano(), 36)}, Env: map[string]string{"FROM_CREATE": "yes"}}
			sb, err := p.Create(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := p.Delete(context.Background(), sb.Reference()); err != nil {
					t.Error(err)
				}
			})
			name, _, err := p.identity(sb.Reference())
			if err != nil {
				t.Fatal(err)
			}

			sandboxtest.Run(t, sb)

			t.Run("startup script", func(t *testing.T) {
				result, err := sb.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sh", "-c", `wc -l </tmp/starts | tr -d ' '; for f in /proc/[0-9]*/cmdline; do tr '\0' ' ' <"$f" 2>/dev/null; echo; done | grep -c '[s]leep 4441'`}})
				if err != nil {
					t.Fatal(err)
				}
				if result.Stdout != "1\n1\n" {
					t.Fatalf("%+v", result)
				}
				broken := req
				broken.Profile = "broken"
				broken.Session.SessionID += "-broken"
				if _, err := p.Create(ctx, broken); err == nil || !strings.Contains(err.Error(), "cannot start") {
					t.Fatalf("err=%v", err)
				}
				pod, err := p.pod(ctx, broken)
				if err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(time.Minute)
				for {
					_, err := client.Pods(namespace).Get(ctx, pod.Name, metav1.GetOptions{})
					if apierrors.IsNotFound(err) {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("failed pod left behind: %v", err)
					}
					time.Sleep(200 * time.Millisecond)
				}
			})
			t.Run("orphans are reaped", func(t *testing.T) {
				result, err := sb.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sh", "-c", `sh -c "sleep 1 >/dev/null 2>&1 & exit 0"; sleep 2; for f in /proc/[0-9]*/stat; do cut -d" " -f3 "$f" 2>/dev/null; done | grep -c Z`}})
				if err != nil {
					t.Fatal(err)
				}
				if strings.TrimSpace(result.Stdout) != "0" {
					t.Fatalf("zombies: %+v", result)
				}
			})
			t.Run("a long command still runs", func(t *testing.T) {
				long := strings.Repeat("x", 120000)
				result, err := sb.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sh", "-c", `printf %s "$0" | wc -c`, long}})
				if err != nil {
					t.Fatal(err)
				}
				if strings.TrimSpace(result.Stdout) != "120000" {
					t.Fatalf("%+v", result)
				}
			})
			t.Run("activity is recorded and protects the pod", func(t *testing.T) {
				if deleted, err := p.DeleteIdle(ctx, time.Hour); err != nil || deleted != 0 {
					t.Fatalf("deleted %d: %v", deleted, err)
				}
				// A second before the sweep, so the recorded time is clearly older.
				time.Sleep(2100 * time.Millisecond)
				if err := sb.(*runtime).KeepAlive(ctx); err != nil {
					t.Fatal(err)
				}
				pod, err := client.Pods(namespace).Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				recorded, err := time.Parse(time.RFC3339, pod.Annotations[activityKey])
				if err != nil || time.Since(recorded) > 10*time.Second {
					t.Fatalf("activity %v: %v", recorded, err)
				}
				if deleted, err := p.DeleteIdle(ctx, time.Minute); err != nil || deleted != 0 {
					t.Fatalf("deleted a pod in use: %d %v", deleted, err)
				}
			})
			t.Run("idle pods are deleted", func(t *testing.T) {
				brief := req
				brief.Session.SessionID += "-brief"
				other, err := p.Create(ctx, brief)
				if err != nil {
					t.Fatal(err)
				}
				otherName, _, _ := p.identity(other.Reference())
				time.Sleep(2100 * time.Millisecond)
				if err := sb.(*runtime).KeepAlive(ctx); err != nil {
					t.Fatal(err)
				}
				if deleted, err := p.DeleteIdle(ctx, time.Second); err != nil || deleted != 1 {
					t.Fatalf("deleted %d: %v", deleted, err)
				}
				if _, err := client.Pods(namespace).Get(ctx, name, metav1.GetOptions{}); err != nil {
					t.Fatal("deleted the pod in use:", err)
				}
				deadline := time.Now().Add(time.Minute)
				for {
					_, err := client.Pods(namespace).Get(ctx, otherName, metav1.GetOptions{})
					if apierrors.IsNotFound(err) {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("idle pod still there: %v", err)
					}
					time.Sleep(200 * time.Millisecond)
				}
				if _, err := p.Connect(ctx, other.Reference()); err == nil {
					t.Fatal("connected to a deleted pod")
				}
			})
			t.Run("max lifetime ends the pod", func(t *testing.T) {
				short, err := New(Config{RESTConfig: config, Namespace: namespace, IdleTimeout: -1, MaxLifetime: 5 * time.Second, ReadyTimeout: time.Minute, Profiles: p.cfg.Profiles})
				if err != nil {
					t.Fatal(err)
				}
				defer short.Close()
				brief := req
				brief.Session.SessionID += "-deadline"
				sb, err := short.Create(ctx, brief)
				if err != nil {
					t.Fatal(err)
				}
				defer short.Delete(context.Background(), sb.Reference())
				deadline := time.Now().Add(time.Minute)
				name, _, _ := short.identity(sb.Reference())
				for {
					pod, err := client.Pods(namespace).Get(ctx, name, metav1.GetOptions{})
					if err != nil {
						t.Fatal(err)
					}
					if pod.Status.Phase == corev1.PodFailed {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("pod outlived its deadline: %s", pod.Status.Phase)
					}
					time.Sleep(200 * time.Millisecond)
				}
			})
		})
	}
}
