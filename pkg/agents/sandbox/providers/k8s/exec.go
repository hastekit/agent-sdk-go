package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox/internal/execsandbox"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
)

// streamFunc opens one exec stream and waits for it to end.
type streamFunc func(context.Context, *url.URL, remotecommand.StreamOptions) error

// openStream prefers WebSockets and falls back to SPDY for an older API server,
// as kubectl does.
func (p *Provider) openStream(ctx context.Context, target *url.URL, options remotecommand.StreamOptions) error {
	websocket, err := remotecommand.NewWebSocketExecutor(p.cfg.RESTConfig, "GET", target.String())
	if err != nil {
		return err
	}
	spdy, err := remotecommand.NewSPDYExecutor(p.cfg.RESTConfig, "POST", target)
	if err != nil {
		return err
	}
	executor, err := remotecommand.NewFallbackExecutor(websocket, spdy, httpstream.IsUpgradeFailure)
	if err != nil {
		return err
	}
	return executor.StreamWithContext(ctx, options)
}

// runner runs processes through the API server's exec subresource, and keeps the
// pod's activity current while it does.
type runner struct {
	provider  *Provider
	name      string
	uid       types.UID
	container string

	mu      sync.Mutex
	renewed time.Time
}

func (r *runner) Run(ctx context.Context, req execsandbox.Request) (int, error) {
	if err := r.renewIfDue(ctx); err != nil {
		return 0, err
	}
	defer r.heartbeat(ctx)()
	// A command's arguments would go in the exec URL, which ends up in the API
	// server's audit logs and is limited in size, so it all goes over stdin.
	finished := make(chan struct{})
	defer close(finished)
	stdin := io.MultiReader(strings.NewReader(req.StdinScript()), waitForClose(finished))
	target := r.provider.execClient.RESTClient().Post().Resource("pods").Namespace(r.provider.cfg.Namespace).Name(r.name).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{Container: r.container, Command: []string{"sh"}, Stdin: true, Stdout: true, Stderr: true}, scheme.ParameterCodec).URL()
	err := r.provider.stream(ctx, target, remotecommand.StreamOptions{Stdin: stdin, Stdout: req.Stdout, Stderr: req.Stderr})
	var exit utilexec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitStatus(), nil
	}
	if err != nil {
		return 0, mapError(err)
	}
	return 0, nil
}

// waitForClose keeps a process's stdin open, since its closing is what tells the
// process that nobody is waiting for it any more.
type waitForClose <-chan struct{}

func (w waitForClose) Read([]byte) (int, error) {
	<-w
	return 0, io.EOF
}

// renew records that the pod is in use. The UID makes this fail for a replacement
// pod that has taken the name.
func (r *runner) renew(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"uid": r.uid, "annotations": map[string]string{activityKey: now.UTC().Format(time.RFC3339)}}})
	if err != nil {
		return err
	}
	if _, err := r.provider.client.Pods(r.provider.cfg.Namespace).Patch(ctx, r.name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return mapError(err)
	}
	r.renewed = now
	return nil
}

func (r *runner) renewIfDue(ctx context.Context) error {
	r.mu.Lock()
	due := time.Since(r.renewed) >= r.provider.renewEvery()
	r.mu.Unlock()
	if !due {
		return nil
	}
	return r.renew(ctx)
}

// heartbeat keeps the pod from looking idle during a long operation, and stops by
// itself with the process that started it. It returns how to stop it.
func (r *runner) heartbeat(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(r.provider.renewEvery())
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := r.renew(ctx); err != nil && ctx.Err() == nil {
					slog.WarnContext(ctx, "failed to record sandbox pod activity", slog.String("pod", r.name), slog.Any("error", err))
				}
			}
		}
	}()
	return cancel
}
