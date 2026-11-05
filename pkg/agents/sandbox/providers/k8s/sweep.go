package k8s

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox/internal/provision"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// renewEvery is how stale recorded activity may get. It is small enough to keep a
// busy pod from looking idle to anyone, and big enough that a burst of tool calls
// does not write to the API server for each one.
func (p *Provider) renewEvery() time.Duration {
	if p.cfg.IdleTimeout <= 0 {
		return time.Minute
	}
	return max(min(p.cfg.IdleTimeout/4, time.Minute), time.Second)
}

// sweep needs no coordination. Activity is read from the pods themselves, so any
// number of providers reach the same answer, and deleting is idempotent.
func (p *Provider) sweep(ctx context.Context) {
	defer close(p.sweepEnded)
	ticker := time.NewTicker(p.renewEvery())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := p.DeleteIdle(ctx, p.cfg.IdleTimeout); err != nil && ctx.Err() == nil {
				slog.WarnContext(ctx, "failed to delete idle sandbox pods", slog.Any("error", err))
			}
		}
	}
}

// DeleteIdle deletes the sandbox pods that have had no activity for longer than
// idle, in any phase, and reports how many it deleted. Everything not on a volume
// goes with them, and a session whose pod is gone fails its next sandbox call
// until its state is cleared.
func (p *Provider) DeleteIdle(ctx context.Context, idle time.Duration) (int, error) {
	if idle < 0 {
		return 0, fmt.Errorf("negative idle duration")
	}
	pods, err := p.client.Pods(p.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: provision.ManagedKey + "=" + provision.ManagedValue})
	if err != nil {
		return 0, mapError(err)
	}
	cutoff := time.Now().Add(-idle)
	deleted := 0
	var failures []error
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil || lastActivity(pod).After(cutoff) {
			continue
		}
		// The resource version makes this fail if the pod has been used since it was
		// listed, which renewing its activity shows up as.
		uid, version := pod.UID, pod.ResourceVersion
		policy := metav1.DeletePropagationBackground
		err := p.client.Pods(p.cfg.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &version}, PropagationPolicy: &policy})
		switch {
		case err == nil:
			deleted++
		case apierrors.IsNotFound(err), apierrors.IsConflict(err):
		default:
			failures = append(failures, fmt.Errorf("pod %s: %w", pod.Name, err))
		}
	}
	return deleted, errors.Join(failures...)
}

// lastActivity falls back to when the pod was created, which is what a pod from
// before activity was recorded has to go on.
func lastActivity(pod *corev1.Pod) time.Time {
	last := pod.CreationTimestamp.Time
	if recorded, err := time.Parse(time.RFC3339, pod.Annotations[activityKey]); err == nil && recorded.After(last) {
		return recorded
	}
	return last
}
