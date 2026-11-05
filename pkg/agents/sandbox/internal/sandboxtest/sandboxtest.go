// Package sandboxtest checks what a provider's exec path promises against a real
// container, which is the only place these behaviours can be observed: they come
// from the container runtime, the shell and the tools in the image.
package sandboxtest

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
)

// Run checks a running sandbox whose workspace is /workspace and whose image
// provides the tools the exec path and file helpers need. FROM_CREATE must be set to "yes" in the
// environment it was created with.
func Run(t *testing.T, sb sandbox.Sandbox) {
	ctx := context.Background()
	exec := func(t *testing.T, r sandbox.ExecRequest) *sandbox.ExecResult {
		t.Helper()
		result, err := sb.Exec(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	running := func(t *testing.T, pattern string) string {
		t.Helper()
		result := exec(t, sandbox.ExecRequest{Argv: []string{"sh", "-c", `for f in /proc/[0-9]*/cmdline; do tr '\0' ' ' <"$f" 2>/dev/null | grep -- "$0" | grep -v grep; done`, pattern}})
		return strings.TrimSpace(result.Stdout)
	}

	t.Run("exec", func(t *testing.T) {
		result := exec(t, sandbox.ExecRequest{Argv: []string{"sh", "-c", `echo out; echo err >&2; printf '%s %s %s' "$PWD" "$FROM_CREATE" "$FROM_EXEC"; exit 7`}, Env: map[string]string{"FROM_EXEC": "a b"}})
		if result.Stdout != "out\n/workspace yes a b" || result.Stderr != "err\n" || result.ExitCode != 7 {
			t.Fatalf("%+v", result)
		}
		for _, literal := range []string{`$(touch injected); * "x"`, "it's", "two\nlines", strings.Repeat("x", 100000)} {
			if got := exec(t, sandbox.ExecRequest{Argv: []string{"printf", "%s", literal}}).Stdout; got != literal {
				t.Fatalf("%.40q became %.40q", literal, got)
			}
		}
		if result := exec(t, sandbox.ExecRequest{Argv: []string{"hastekit-not-a-command"}}); result.ExitCode != 127 {
			t.Fatalf("%+v", result)
		}
		if result := exec(t, sandbox.ExecRequest{Argv: []string{"true"}, Workdir: "no/such/dir"}); result.ExitCode == 0 {
			t.Fatalf("%+v", result)
		}
	})
	t.Run("timeout kills the group", func(t *testing.T) {
		result := exec(t, sandbox.ExecRequest{Argv: []string{"sh", "-c", "sleep 1111 & sleep 1112"}, Timeout: time.Second})
		if result.ExitCode != 124 {
			t.Fatalf("%+v", result)
		}
		if left := running(t, "sleep 111"); left != "" {
			t.Fatalf("still running: %q", left)
		}
	})
	t.Run("cancel kills the group", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := sb.Exec(cancelled, sandbox.ExecRequest{Argv: []string{"sh", "-c", "sleep 2221 & sleep 2222"}, Timeout: time.Minute})
			done <- err
		}()
		deadline := time.Now().Add(10 * time.Second)
		for running(t, "sleep 222") == "" {
			if time.Now().After(deadline) {
				t.Fatal("command never started")
			}
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
		deadline = time.Now().Add(10 * time.Second)
		for running(t, "sleep 222") != "" {
			if time.Now().After(deadline) {
				t.Fatal("cancelled command is still running")
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	t.Run("background job survives", func(t *testing.T) {
		start := time.Now()
		result := exec(t, sandbox.ExecRequest{Argv: []string{"sh", "-c", "sleep 3331 >/dev/null 2>&1 & echo started"}, Timeout: time.Second})
		if result.ExitCode != 0 || result.Stdout != "started\n" {
			t.Fatalf("%+v", result)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("waited %v for the background job", elapsed)
		}
		time.Sleep(1500 * time.Millisecond)
		if running(t, "sleep 3331") == "" {
			t.Fatal("background job was killed")
		}
	})
	t.Run("job holding stdout does not hold up the command", func(t *testing.T) {
		start := time.Now()
		result := exec(t, sandbox.ExecRequest{Argv: []string{"sh", "-c", "sleep 15 & echo started"}})
		if result.ExitCode != 0 || result.Stdout != "started\n" {
			t.Fatalf("%+v", result)
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Fatalf("waited %v for the background job", elapsed)
		}
	})
	t.Run("output limit", func(t *testing.T) {
		start := time.Now()
		result := exec(t, sandbox.ExecRequest{Argv: []string{"yes"}, Timeout: time.Minute})
		if result.ExitCode != 137 || !result.OutputTruncated || !strings.Contains(result.Stderr, "output limit") {
			t.Fatalf("exit=%d truncated=%v stderr=%q", result.ExitCode, result.OutputTruncated, result.Stderr)
		}
		if elapsed := time.Since(start); elapsed > 45*time.Second {
			t.Fatalf("took %v", elapsed)
		}
		deadline := time.Now().Add(10 * time.Second)
		for running(t, "^yes") != "" {
			if time.Now().After(deadline) {
				t.Fatal("yes is still running")
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	t.Run("files", func(t *testing.T) {
		payload := append([]byte{0, 255, 128, '\n', 'K'}, bytes.Repeat([]byte{0, 1, 2, 254}, 100<<10)...)
		if err := sandbox.WriteFile(ctx, sb, "deep/er/a b's.bin", payload); err != nil {
			t.Fatal(err)
		}
		got, err := sandbox.ReadFile(ctx, sb, "/workspace/deep/er/a b's.bin")
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("got %d bytes of %d: %v", len(got), len(payload), err)
		}
		if result := exec(t, sandbox.ExecRequest{Argv: []string{"ls", "-A", "deep/er"}}); result.Stdout != "a b's.bin\n" {
			t.Fatalf("left behind: %q", result.Stdout)
		}
		if err := sandbox.WriteFile(ctx, sb, "empty", nil); err != nil {
			t.Fatal(err)
		}
		if got, err := sandbox.ReadFile(ctx, sb, "empty"); err != nil || len(got) != 0 {
			t.Fatalf("%q %v", got, err)
		}
		if _, err := sandbox.ReadFile(ctx, sb, "missing"); !errors.Is(err, sandbox.ErrFileNotFound) {
			t.Fatalf("err=%v", err)
		}
		exec(t, sandbox.ExecRequest{Argv: []string{"sh", "-c", `dd if=/dev/zero of=full bs="$0" count=1 2>/dev/null`, strconv.Itoa(sandbox.MaxReadFile)}})
		if got, err := sandbox.ReadFile(ctx, sb, "full"); err != nil || len(got) != sandbox.MaxReadFile {
			t.Fatalf("largest file: %d bytes: %v", len(got), err)
		}
		exec(t, sandbox.ExecRequest{Argv: []string{"sh", "-c", `echo more >>full`}})
		if _, err := sandbox.ReadFile(ctx, sb, "full"); !errors.Is(err, sandbox.ErrFileTooLarge) {
			t.Fatalf("err=%v", err)
		}
		exec(t, sandbox.ExecRequest{Argv: []string{"ln", "-s", "/etc", "out"}})
		if _, err := sandbox.ReadFile(ctx, sb, "out/passwd"); err == nil {
			t.Fatal("read through a symlink out of the workspace")
		}
		if _, err := sandbox.ReadFile(ctx, sb, "/etc/passwd"); err == nil {
			t.Fatal("read outside the workspace")
		}
		if err := sandbox.WriteFile(ctx, sb, "out/hastekit-escape", []byte("x")); err == nil {
			t.Fatal("wrote through a symlink out of the workspace")
		}
		if err := sandbox.RemoveFile(ctx, sb, "deep/er/a b's.bin"); err != nil {
			t.Fatal(err)
		}
		if err := sandbox.RemoveFile(ctx, sb, "deep/er/a b's.bin"); !errors.Is(err, sandbox.ErrFileNotFound) {
			t.Fatalf("err=%v", err)
		}
	})
}
