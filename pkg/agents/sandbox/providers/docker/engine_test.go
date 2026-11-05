package docker

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox/internal/sandboxtest"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// TestRealEngine runs what the fixture can only imitate against a Docker engine,
// once for each image in SANDBOX_DOCKER_TEST_IMAGES, a comma-separated list.
// Images differ in the shell and tools they have, so cover both kinds:
//
//	SANDBOX_DOCKER_TEST_IMAGES=debian:bookworm-slim,alpine:3.20 go test ./pkg/agents/sandbox/providers/docker/
func TestRealEngine(t *testing.T) {
	images := os.Getenv("SANDBOX_DOCKER_TEST_IMAGES")
	if images == "" {
		t.Skip("SANDBOX_DOCKER_TEST_IMAGES is not set")
	}
	for _, image := range strings.Split(images, ",") {
		t.Run(image, func(t *testing.T) { testRealEngine(t, strings.TrimSpace(image)) })
	}
}

func testRealEngine(t *testing.T, image string) {
	engine, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	// The README's script for skills, a service, and a record of each time it ran.
	startup := `mkdir -p /usr/local/bin
for file in /skills/*/bin/*; do
	[ -f "$file" ] && ln -s "$file" /usr/local/bin/ 2>/dev/null
done
sleep 4441 >/dev/null 2>&1 &
echo started >>/tmp/starts
`
	p, err := New(Config{Client: engine, IdleTimeout: -1, Profiles: map[string]Profile{
		"base":   {Config: container.Config{Image: image}, PullPolicy: PullNever, StartupScript: startup},
		"broken": {Config: container.Config{Image: image}, PullPolicy: PullNever, StartupScript: "echo cannot start >&2; exit 3"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	ctx := context.Background()
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
	exec := func(t *testing.T, r sandbox.ExecRequest) *sandbox.ExecResult {
		t.Helper()
		result, err := sb.Exec(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	sandboxtest.Run(t, sb)
	t.Run("idle containers stop and resume with their files", func(t *testing.T) {
		if err := sandbox.WriteFile(ctx, sb, "survivor", []byte("still here")); err != nil {
			t.Fatal(err)
		}
		exec(t, sandbox.ExecRequest{Argv: []string{"sh", "-c", `mkdir -p /skills/demo/bin && printf '#!/bin/sh\necho skill ran\n' >/skills/demo/bin/hastekit-demo && chmod +x /skills/demo/bin/hastekit-demo`}})
		isRunning := func() bool {
			t.Helper()
			inspected, err := engine.ContainerInspect(ctx, sb.Reference().ID, client.ContainerInspectOptions{})
			if err != nil {
				t.Fatal(err)
			}
			return inspected.Container.State.Running
		}
		if _, err := p.StopIdle(ctx, time.Hour); err != nil || !isRunning() {
			t.Fatalf("stopped a container in use: %v", err)
		}
		// A command in flight keeps a container from being stopped, however old the
		// last activity it recorded when it began.
		long := make(chan *sandbox.ExecResult, 1)
		go func() {
			result, err := sb.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sleep", "4"}})
			if err != nil {
				t.Error(err)
			}
			long <- result
		}()
		time.Sleep(2500 * time.Millisecond)
		if _, err := p.StopIdle(ctx, time.Second); err != nil || !isRunning() {
			t.Fatalf("stopped a container mid-command: %v", err)
		}
		if result := <-long; result == nil || result.ExitCode != 0 {
			t.Fatalf("command in flight failed: %+v", result)
		}
		time.Sleep(1100 * time.Millisecond)
		stopped, err := p.StopIdle(ctx, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if stopped == 0 || isRunning() {
			t.Fatalf("idle container kept running, stopped %d", stopped)
		}
		inspected, err := engine.ContainerInspect(ctx, sb.Reference().ID, client.ContainerInspectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if inspected.Container.State.ExitCode == 137 && inspected.Container.State.OOMKilled {
			t.Fatal("container was killed rather than stopped")
		}
		resumed, err := p.Connect(ctx, sb.Reference())
		if err != nil {
			t.Fatal(err)
		}
		got, err := sandbox.ReadFile(ctx, resumed, "survivor")
		if err != nil || string(got) != "still here" {
			t.Fatalf("%q %v", got, err)
		}
		result, err := resumed.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sh", "-c", `hastekit-demo; echo $FROM_CREATE; wc -l </tmp/starts | tr -d ' '; for f in /proc/[0-9]*/cmdline; do tr '\0' ' ' <"$f" 2>/dev/null; echo; done | grep -c '[s]leep 4441'`}})
		if err != nil {
			t.Fatal(err)
		}
		if result.Stdout != "skill ran\nyes\n2\n1\n" {
			t.Fatalf("%+v", result)
		}
	})
	t.Run("failed startup script", func(t *testing.T) {
		broken := req
		broken.Profile = "broken"
		broken.Session.SessionID += "-broken"
		if _, err := p.Create(ctx, broken); err == nil || !strings.Contains(err.Error(), "cannot start") {
			t.Fatalf("err=%v", err)
		}
		options, _, err := p.options(ctx, broken)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.ContainerInspect(ctx, options.Name, client.ContainerInspectOptions{}); err == nil {
			t.Fatal("failed container left behind")
		}
	})
	t.Run("max lifetime ends the container", func(t *testing.T) {
		short, err := New(Config{Client: engine, IdleTimeout: -1, MaxLifetime: time.Second, Profiles: map[string]Profile{"base": {Config: container.Config{Image: image}, PullPolicy: PullNever}}})
		if err != nil {
			t.Fatal(err)
		}
		defer short.Close()
		brief := req
		brief.Session.SessionID += "-brief"
		sb, err := short.Create(ctx, brief)
		if err != nil {
			t.Fatal(err)
		}
		defer short.Delete(context.Background(), sb.Reference())
		deadline := time.Now().Add(15 * time.Second)
		for {
			inspected, err := engine.ContainerInspect(ctx, sb.Reference().ID, client.ContainerInspectOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !inspected.Container.State.Running {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("container outlived its max lifetime")
			}
			time.Sleep(100 * time.Millisecond)
		}
		if _, err := short.Connect(ctx, sb.Reference()); err != nil {
			t.Fatal("restart after max lifetime:", err)
		}
	})
}
