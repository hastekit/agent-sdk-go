package execsandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
)

// localRunner runs the scripts in a real shell, set up the way a container runtime
// sets up an exec'd process: leading its own session, with stdin closed when the
// caller goes away. Like containerd, it keeps the stream open for as long as
// anything holds stdout, and only cancelling ends the call early.
type localRunner struct {
	shell string
	// overStdin sends the script as StdinScript, as Kubernetes does.
	overStdin bool
}

func (r localRunner) Run(ctx context.Context, req Request) (int, error) {
	cmd := exec.Command(r.shell, req.Argv()[1:]...)
	if r.overStdin {
		cmd = exec.Command(r.shell)
	}
	cmd.Stdout, cmd.Stderr = req.Stdout, req.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 0, err
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	if r.overStdin {
		go io.WriteString(stdin, req.StdinScript())
	}
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()
	select {
	case <-ctx.Done():
		stdin.Close()
		return 0, ctx.Err()
	case err = <-finished:
	}
	stdin.Close()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal()), nil
		}
		return exit.ExitCode(), nil
	}
	return 0, err
}

type fixture struct {
	*Sandbox
	root, outside, activity string
}

// eachShell runs a test against every POSIX shell available here, since images
// differ in which one they have, and both ways a script can reach it.
func eachShell(t *testing.T, test func(t *testing.T, f fixture)) {
	t.Helper()
	for _, shell := range []string{"sh", "dash", "bash"} {
		path, err := exec.LookPath(shell)
		if err != nil {
			continue
		}
		for mode, overStdin := range map[string]bool{"argv": false, "stdin": true} {
			t.Run(shell+"/"+mode, func(t *testing.T) {
				t.Parallel()
				base, err := filepath.EvalSymlinks(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				f := fixture{root: filepath.Join(base, "workspace"), outside: filepath.Join(base, "outside"), activity: filepath.Join(base, "activity")}
				for _, dir := range []string{f.root, f.outside} {
					if err := os.Mkdir(dir, 0755); err != nil {
						t.Fatal(err)
					}
				}
				f.Sandbox = New(Config{Reference: sandbox.Reference{Provider: "test", ID: "abc"}, Runner: localRunner{shell: path, overStdin: overStdin}, Root: f.root, ActivityFile: f.activity})
				test(t, f)
			})
		}
	}
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d is still running", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func readPID(t *testing.T, text string) int {
	t.Helper()
	pid, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil {
		t.Fatalf("no pid in %q", text)
	}
	return pid
}

func TestExecPassesArgumentsLiterally(t *testing.T) {
	eachShell(t, func(t *testing.T, f fixture) {
		literal := `$(touch injected) "quoted" 'single' ; & *`
		result, err := f.Exec(context.Background(), sandbox.ExecRequest{Argv: []string{"printf", "%s", literal}})
		if err != nil {
			t.Fatal(err)
		}
		if result.Stdout != literal || result.ExitCode != 0 {
			t.Fatalf("%+v", result)
		}
		if _, err := os.Stat(filepath.Join(f.root, "injected")); err == nil {
			t.Fatal("an argument was interpreted")
		}
	})
}

func TestExecKeepsStreamsAndExitCodeApart(t *testing.T) {
	eachShell(t, func(t *testing.T, f fixture) {
		result, err := f.Exec(context.Background(), sandbox.ExecRequest{Argv: []string{"sh", "-c", "echo out; echo err >&2; exit 7"}})
		if err != nil {
			t.Fatal(err)
		}
		if result.Stdout != "out\n" || result.Stderr != "err\n" || result.ExitCode != 7 {
			t.Fatalf("%+v", result)
		}
	})
}

func TestExecResolvesWorkdirAgainstTheWorkspace(t *testing.T) {
	eachShell(t, func(t *testing.T, f fixture) {
		ctx := context.Background()
		if err := os.Mkdir(filepath.Join(f.root, "sub"), 0755); err != nil {
			t.Fatal(err)
		}
		for workdir, want := range map[string]string{"": f.root, "sub": filepath.Join(f.root, "sub"), filepath.Join(f.root, "sub"): filepath.Join(f.root, "sub")} {
			result, err := f.Exec(ctx, sandbox.ExecRequest{Argv: []string{"pwd"}, Workdir: workdir})
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(result.Stdout) != want {
				t.Fatalf("workdir %q ran in %q", workdir, result.Stdout)
			}
		}
		if _, err := f.Exec(ctx, sandbox.ExecRequest{Argv: []string{"pwd"}, Workdir: "../outside"}); err == nil {
			t.Fatal("ran outside the workspace")
		}
		result, err := f.Exec(ctx, sandbox.ExecRequest{Argv: []string{"pwd"}, Workdir: "missing"})
		if err != nil {
			t.Fatal(err)
		}
		if result.ExitCode == 0 || !strings.Contains(result.Stderr, "missing") {
			t.Fatalf("%+v", result)
		}
	})
}

func TestExecPassesEnvironment(t *testing.T) {
	eachShell(t, func(t *testing.T, f fixture) {
		result, err := f.Exec(context.Background(), sandbox.ExecRequest{Argv: []string{"sh", "-c", `printf %s "$VALUE"`}, Env: map[string]string{"VALUE": "a b $c"}})
		if err != nil {
			t.Fatal(err)
		}
		if result.Stdout != "a b $c" {
			t.Fatalf("%+v", result)
		}
	})
}

func TestExecRunsAnExecutableAndNeverAShellBuiltin(t *testing.T) {
	eachShell(t, func(t *testing.T, f fixture) {
		result, err := f.Exec(context.Background(), sandbox.ExecRequest{Argv: []string{"hastekit-not-a-command"}})
		if err != nil {
			t.Fatal(err)
		}
		if result.ExitCode != 127 {
			t.Fatalf("%+v", result)
		}
		// `shift` is a special builtin with no executable behind it. Run by the
		// wrapper it would succeed, and take the wrapper's arguments with it.
		result, err = f.Exec(context.Background(), sandbox.ExecRequest{Argv: []string{"shift"}})
		if err != nil {
			t.Fatal(err)
		}
		if result.ExitCode != 127 {
			t.Fatalf("%+v", result)
		}
	})
}

func TestExecTimeoutKillsTheWholeProcessGroup(t *testing.T) {
	eachShell(t, func(t *testing.T, f fixture) {
		start := time.Now()
		result, err := f.Exec(context.Background(), sandbox.ExecRequest{Argv: []string{"sh", "-c", "sleep 30 & echo $!; wait"}, Timeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		if result.ExitCode != 124 {
			t.Fatalf("%+v", result)
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Fatalf("took %v", elapsed)
		}
		waitGone(t, readPID(t, result.Stdout))
	})
}

func TestCancelKillsTheWholeProcessGroup(t *testing.T) {
	eachShell(t, func(t *testing.T, f fixture) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := f.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sh", "-c", "sleep 30 & echo $! > pid; wait"}, Timeout: time.Minute})
			done <- err
		}()
		var pid []byte
		for len(pid) == 0 {
			select {
			case err := <-done:
				t.Fatalf("finished early: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			pid, _ = os.ReadFile(filepath.Join(f.root, "pid"))
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
		waitGone(t, readPID(t, string(pid)))
	})
}

// A background job that keeps stdout open keeps the stream open too, and some
// container runtimes then never report that the command has ended.
func TestBackgroundJobHoldingStdoutDoesNotHoldUpTheCommand(t *testing.T) {
	eachShell(t, func(t *testing.T, f fixture) {
		start := time.Now()
		result, err := f.Exec(context.Background(), sandbox.ExecRequest{Argv: []string{"sh", "-c", "sleep 20 & echo $!; echo err >&2; exit 5"}})
		if err != nil {
			t.Fatal(err)
		}
		pid := readPID(t, result.Stdout)
		t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
		if result.ExitCode != 5 || result.Stderr != "err\n" {
			t.Fatalf("%+v", result)
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Fatalf("waited %v for the background job", elapsed)
		}
		if !alive(pid) {
			t.Fatal("background job was killed")
		}
	})
}

// A job in its own session escapes the process group, so the timer cannot kill it
// and it can hold the stream long after the command is gone.
func TestTimeoutIsReportedEvenIfTheStreamNeverEnds(t *testing.T) {
	path, err := exec.LookPath("setsid")
	if err != nil {
		t.Skip("no setsid here")
	}
	eachShell(t, func(t *testing.T, f fixture) {
		start := time.Now()
		result, err := f.Exec(context.Background(), sandbox.ExecRequest{Argv: []string{"sh", "-c", path + " sleep 25 & sleep 20"}, Timeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		if result.ExitCode != 124 {
			t.Fatalf("%+v", result)
		}
		if elapsed := time.Since(start); elapsed > 15*time.Second {
			t.Fatalf("took %v", elapsed)
		}
	})
}

func TestOutputAroundTheExitMarkerSurvives(t *testing.T) {
	eachShell(t, func(t *testing.T, f fixture) {
		for _, script := range []string{"printf no-newline", "printf 'ends with newline\\n'", "printf hastekit-exit-", ":"} {
			result, err := f.Exec(context.Background(), sandbox.ExecRequest{Argv: []string{"sh", "-c", script}})
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"printf no-newline": "no-newline", "printf 'ends with newline\\n'": "ends with newline\n", "printf hastekit-exit-": "hastekit-exit-", ":": ""}[script]
			if result.Stdout != want || result.ExitCode != 0 {
				t.Fatalf("%q: %+v", script, result)
			}
		}
	})
}

func TestExitMarkerAcrossWrites(t *testing.T) {
	var out bytes.Buffer
	ended := 0
	m := newExitMarker(&out, func() { ended++ })
	whole := "before " + m.id + "42;after"
	for i := range whole {
		m.Write([]byte{whole[i]})
	}
	m.flush()
	if done, code := m.result(); !done || code != 42 || out.String() != "before " || ended != 1 {
		t.Fatalf("done=%v code=%d out=%q ended=%d", done, code, out.String(), ended)
	}
	out.Reset()
	m = newExitMarker(&out, func() {})
	m.Write([]byte("no marker " + m.id[:5]))
	m.flush()
	if done, _ := m.result(); done || out.String() != "no marker "+m.id[:5] {
		t.Fatalf("done=%v out=%q", done, out.String())
	}
}

// Everything in a request has to reach the shell intact however it travels.
func TestStdinScriptPreservesArguments(t *testing.T) {
	eachShell(t, func(t *testing.T, f fixture) {
		for _, literal := range []string{"it's", "two\nlines", "$HOME `id` \\ \"q\"", " spaces ", strings.Repeat("x", 100000)} {
			result, err := f.Exec(context.Background(), sandbox.ExecRequest{Argv: []string{"printf", "%s", literal}})
			if err != nil {
				t.Fatal(err)
			}
			if result.Stdout != literal {
				t.Fatalf("%.40q became %.40q", literal, result.Stdout)
			}
		}
	})
}

// A command that starts a server and returns has not failed, and the timeout that
// applied to it must not come back for the server later.
func TestBackgroundJobOutlivesTheCommandAndItsTimeout(t *testing.T) {
	eachShell(t, func(t *testing.T, f fixture) {
		result, err := f.Exec(context.Background(), sandbox.ExecRequest{Argv: []string{"sh", "-c", "sleep 30 >/dev/null 2>&1 & echo $!"}, Timeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		pid := readPID(t, result.Stdout)
		t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
		if result.ExitCode != 0 {
			t.Fatalf("%+v", result)
		}
		time.Sleep(1500 * time.Millisecond)
		if !alive(pid) {
			t.Fatal("background job was killed")
		}
	})
}

func TestExecTruncatesOutputPastTheCap(t *testing.T) {
	eachShell(t, func(t *testing.T, f fixture) {
		result, err := f.Exec(context.Background(), sandbox.ExecRequest{Argv: []string{"sh", "-c", "head -c 5242880 /dev/zero; exit 3"}})
		if err != nil {
			t.Fatal(err)
		}
		if !result.OutputTruncated || result.ExitCode != 3 || len(result.Stdout) > maxOutput+100 || !strings.HasSuffix(result.Stdout, "[output truncated]") {
			t.Fatalf("truncated=%v exit=%d size=%d", result.OutputTruncated, result.ExitCode, len(result.Stdout))
		}
	})
}

func TestExecKillsACommandThatWillNotStopTalking(t *testing.T) {
	eachShell(t, func(t *testing.T, f fixture) {
		start := time.Now()
		result, err := f.Exec(context.Background(), sandbox.ExecRequest{Argv: []string{"yes"}, Timeout: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed > 30*time.Second {
			t.Fatalf("took %v", elapsed)
		}
		if result.ExitCode != exitKilled || !result.OutputTruncated || !strings.Contains(result.Stderr, "output limit") {
			t.Fatalf("exit=%d truncated=%v stderr=%q", result.ExitCode, result.OutputTruncated, result.Stderr)
		}
	})
}

func TestEveryOperationCountsAsActivity(t *testing.T) {
	eachShell(t, func(t *testing.T, f fixture) {
		ctx := context.Background()
		stale := time.Now().Add(-time.Hour)
		operations := map[string]func() error{
			"exec": func() error {
				_, err := f.Exec(ctx, sandbox.ExecRequest{Argv: []string{"true"}})
				return err
			},
			"keep alive": func() error { return f.KeepAlive(ctx) },
		}
		for name, operation := range operations {
			if err := os.WriteFile(f.activity, nil, 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(f.activity, stale, stale); err != nil {
				t.Fatal(err)
			}
			if err := operation(); err != nil {
				t.Fatal(name, err)
			}
			info, err := os.Stat(f.activity)
			if err != nil {
				t.Fatal(name, err)
			}
			if info.ModTime().Before(time.Now().Add(-time.Minute)) {
				t.Errorf("%s did not count as activity", name)
			}
		}
	})
}

// A long command must not look idle the moment it finishes.
func TestExecCountsAsActivityWhenItEnds(t *testing.T) {
	eachShell(t, func(t *testing.T, f fixture) {
		started := time.Now()
		_, err := f.Exec(context.Background(), sandbox.ExecRequest{Argv: []string{"sleep", "2"}})
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(f.activity)
		if err != nil {
			t.Fatal(err)
		}
		if info.ModTime().Before(started.Add(time.Second)) {
			t.Fatalf("activity at %v, command started at %v", info.ModTime(), started)
		}
	})
}

func TestStartupRunsAScriptInTheWorkspace(t *testing.T) {
	eachShell(t, func(t *testing.T, f fixture) {
		if err := f.Startup(context.Background(), `pwd >ran`, time.Minute); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(f.root, "ran"))
		if err != nil || strings.TrimSpace(string(got)) != f.root {
			t.Fatalf("%q %v", got, err)
		}
	})
}

func TestStartupWithNoScriptRunsNothing(t *testing.T) {
	s := New(Config{Runner: failedRunner{}, Root: "/workspace"})
	if err := s.Startup(context.Background(), "", time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestStartupFailureSaysWhy(t *testing.T) {
	eachShell(t, func(t *testing.T, f fixture) {
		err := f.Startup(context.Background(), "echo progress; echo 'cannot reach registry' >&2; exit 3", time.Minute)
		if err == nil || !strings.Contains(err.Error(), "exit code 3") || !strings.Contains(err.Error(), "cannot reach registry") || !strings.Contains(err.Error(), "progress") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestStartupStopsAtItsTimeout(t *testing.T) {
	eachShell(t, func(t *testing.T, f fixture) {
		err := f.Startup(context.Background(), "sleep 30", time.Second)
		if err == nil || !strings.Contains(err.Error(), "exit code 124") {
			t.Fatalf("err=%v", err)
		}
	})
}

// Starting a service is the obvious thing to want from a startup script.
func TestServiceStartedAtStartupKeepsRunning(t *testing.T) {
	eachShell(t, func(t *testing.T, f fixture) {
		if err := f.Startup(context.Background(), "sleep 30 >/dev/null 2>&1 & echo $! >service", time.Second); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(f.root, "service"))
		if err != nil {
			t.Fatal(err)
		}
		pid := readPID(t, string(data))
		t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
		time.Sleep(1500 * time.Millisecond)
		if !alive(pid) {
			t.Fatal("service was killed")
		}
	})
}

// failedRunner is a provider reporting that the process never started, as it does
// for an image without sh.
type failedRunner struct{}

func (failedRunner) Run(_ context.Context, req Request) (int, error) {
	io.WriteString(req.Stdout, "OCI runtime exec failed: sh not found")
	return 127, nil
}

func TestProviderFailureIsReported(t *testing.T) {
	s := New(Config{Runner: failedRunner{}, Root: "/workspace"})
	if err := s.KeepAlive(context.Background()); err == nil || !strings.Contains(err.Error(), "sh not found") {
		t.Fatalf("err=%v", err)
	}
}
