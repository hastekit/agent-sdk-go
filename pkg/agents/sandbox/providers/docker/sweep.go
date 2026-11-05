package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/containerd/errdefs"
	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox/internal/execsandbox"
	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox/internal/provision"
	"github.com/moby/moby/client"
)

const kubernetesPodLabel = "io.kubernetes.pod.name"

// sweep needs no coordination. Activity is read from the containers themselves,
// so any number of providers reach the same answer, and stopping is idempotent.
func (p *Provider) sweep(ctx context.Context) {
	defer close(p.sweepEnded)
	ticker := time.NewTicker(max(min(p.cfg.IdleTimeout/4, time.Minute), time.Second))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := p.StopIdle(ctx, p.cfg.IdleTimeout); err != nil && ctx.Err() == nil {
				slog.WarnContext(ctx, "failed to stop idle sandbox containers", slog.Any("error", err))
			}
		}
	}
}

// StopIdle stops the running sandbox containers that have had no activity for
// longer than idle, and reports how many it stopped. A stopped container keeps
// its files and installed packages but not its processes; Connect starts it again.
func (p *Provider) StopIdle(ctx context.Context, idle time.Duration) (int, error) {
	if idle < 0 {
		return 0, fmt.Errorf("negative idle duration")
	}
	filters := make(client.Filters).Add("label", provision.ManagedKey+"="+provision.ManagedValue).Add("status", "running")
	listed, err := p.engine.ContainerList(ctx, client.ContainerListOptions{Filters: filters})
	if err != nil {
		return 0, mapError(err)
	}
	cutoff := time.Now().Add(-idle)
	stopped := 0
	var failures []error
	for _, item := range listed.Items {
		// Kubernetes on this engine copies a pod's labels onto its containers, and
		// those belong to the Kubernetes provider.
		if _, isPod := item.Labels[kubernetesPodLabel]; isPod {
			continue
		}
		active, err := p.activeSince(ctx, item.ID, cutoff)
		if err == nil && !active {
			if _, err = p.engine.ContainerStop(ctx, item.ID, client.ContainerStopOptions{}); err == nil {
				stopped++
			}
		}
		if err != nil && !errdefs.IsNotFound(err) {
			failures = append(failures, fmt.Errorf("container %s: %w", item.ID, err))
		}
	}
	return stopped, errors.Join(failures...)
}

// activeSince reports whether a container has been used since cutoff or is in the
// middle of something now. A container counts from when it started, because
// something started it in order to use it, and a read-only filesystem leaves
// that as all there is to go on.
func (p *Provider) activeSince(ctx context.Context, id string, cutoff time.Time) (bool, error) {
	inspected, err := p.engine.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return false, err
	}
	info := inspected.Container
	if info.State != nil {
		if started, err := time.Parse(time.RFC3339Nano, info.State.StartedAt); err == nil && started.After(cutoff) {
			return true, nil
		}
	}
	activity, err := p.engine.ContainerStatPath(ctx, id, client.ContainerStatPathOptions{Path: execsandbox.DefaultActivityFile})
	if err != nil && !errdefs.IsNotFound(err) {
		return false, err
	}
	if err == nil && activity.Stat.Mtime.After(cutoff) {
		return true, nil
	}
	for _, execID := range info.ExecIDs {
		exec, err := p.engine.ExecInspect(ctx, execID, client.ExecInspectOptions{})
		if err != nil && !errdefs.IsNotFound(err) {
			return false, err
		}
		if err == nil && exec.Running {
			return true, nil
		}
	}
	return false, nil
}
