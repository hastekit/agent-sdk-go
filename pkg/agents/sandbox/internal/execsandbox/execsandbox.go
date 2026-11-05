// Package execsandbox implements sandbox.Sandbox over a provider's native exec,
// for containers that run no process of ours. A provider supplies the Runner;
// this package owns what a command means: literal arguments, the workspace,
// timeouts and output limits.
//
// Images must provide a POSIX sh with cat and sleep. touch is used when present.
package execsandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
)

const (
	// DefaultActivityFile is touched by every operation. Its modification time is
	// the sandbox's last activity, readable by whoever decides when it is idle.
	DefaultActivityFile = "/tmp/.hastekit-activity"

	maxOutput = 4 << 20

	// maxDrain is how much output a command may produce before it is killed.
	// Output past maxOutput is discarded, but it still crosses the provider's
	// exec stream, so a command like `yes` must not run for its whole timeout.
	maxDrain = 64 << 20

	// execGrace lets the timer inside the sandbox fire first. Waiting longer
	// than this means the command is dead but something holds its stream open.
	execGrace = 10 * time.Second

	// execDrain is how long a finished command's stream may stay open, for the
	// last of its stderr to arrive. A background job can hold it open for as long
	// as it lives, and that must not hold up the command that started it.
	execDrain = 2 * time.Second

	exitKilled = 137
)

// Request describes one process to run in the sandbox: a shell script, with Args
// as its positional parameters.
type Request struct {
	Script         string
	Args           []string
	Stdout, Stderr io.Writer
}

// Argv is the request as a command line.
func (r Request) Argv() []string {
	return append([]string{"sh", "-c", r.Script, "sandbox"}, r.Args...)
}

// StdinScript is the request as input for a bare sh, for a provider whose command
// lines are limited in size or end up in logs. It is one block, so the shell has
// read all of it before anything runs, and nothing the script starts can consume
// it from stdin.
func (r Request) StdinScript() string {
	return "{\nset -- " + sandbox.QuoteArgv(r.Args) + "\n" + r.Script + "\nexit $?\n}\n"
}

// Runner starts a process through the provider and waits for its exit code.
// Without a TTY, stdout and stderr stay separate and binary safe. The process's
// stdin stays open for the whole call, and must see EOF when ctx is cancelled or
// the calling process dies, since that is how commands find out that nobody is
// waiting for them. Cancelling ctx must also end the call, even if something still
// holds the process's stdout.
type Runner interface {
	Run(ctx context.Context, req Request) (int, error)
}

type Config struct {
	Reference sandbox.Reference
	Runner    Runner
	// Root is the workspace, the default working directory. Exec's Workdir has to
	// be inside it, but a shell is not confined by it.
	Root string
	// ActivityFile defaults to DefaultActivityFile.
	ActivityFile string
}

type Sandbox struct {
	ref      sandbox.Reference
	runner   Runner
	root     string
	activity string
}

func New(cfg Config) *Sandbox {
	if cfg.ActivityFile == "" {
		cfg.ActivityFile = DefaultActivityFile
	}
	return &Sandbox{ref: cfg.Reference, runner: cfg.Runner, root: path.Clean(cfg.Root), activity: cfg.ActivityFile}
}

func (s *Sandbox) Reference() sandbox.Reference { return s.ref }

func (s *Sandbox) resolve(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return s.root, nil
	}
	target := path.Clean(name)
	if !path.IsAbs(target) {
		target = path.Join(s.root, target)
	}
	if target != s.root && !strings.HasPrefix(target, strings.TrimSuffix(s.root, "/")+"/") {
		return "", fmt.Errorf("path %q is outside the sandbox workspace; use a path under %s", name, s.root)
	}
	return target, nil
}

func (s *Sandbox) Exec(ctx context.Context, req sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	timeout, err := sandbox.ValidateExec(req)
	if err != nil {
		return nil, err
	}
	workdir, err := s.resolve(req.Workdir)
	if err != nil {
		return nil, err
	}
	bounded, cancel := context.WithTimeout(ctx, timeout+execGrace)
	defer cancel()
	running, stop := context.WithCancel(bounded)
	defer stop()
	limit := &drainLimit{exceeded: stop}
	stdout, stderr := &cappedOutput{limit: limit}, &cappedOutput{limit: limit}
	marker := newExitMarker(stdout, func() { time.AfterFunc(execDrain, stop) })
	seconds := strconv.FormatInt(int64((timeout+time.Second-1)/time.Second), 10)
	args := append([]string{workdir, seconds, s.activity, marker.id}, commandLine(req)...)
	start := time.Now()
	code, err := s.runner.Run(running, Request{Script: execScript, Args: args, Stdout: marker, Stderr: stderr})
	elapsed := time.Since(start)
	marker.flush()
	if ended, exit := marker.result(); ended {
		code = exit
	} else {
		switch {
		case limit.wasExceeded():
			code = exitKilled
			stderr.note("[command killed: output limit exceeded]")
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case bounded.Err() != nil:
			// The timer has fired, so the command is gone, but something it left
			// behind in a new session still holds the stream.
			code = 124
		case err != nil:
			return nil, err
		case code == exitKilled && elapsed >= timeout:
			code = 124
		}
	}
	return &sandbox.ExecResult{OutputTruncated: stdout.isTruncated() || stderr.isTruncated(), Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: code, DurationMilli: elapsed.Milliseconds()}, nil
}

// commandLine carries the environment as arguments to env, because not every
// provider's exec API has a field for it.
func commandLine(req sandbox.ExecRequest) []string {
	if len(req.Env) == 0 {
		return req.Argv
	}
	line := []string{"env"}
	for _, key := range slices.Sorted(maps.Keys(req.Env)) {
		line = append(line, key+"="+req.Env[key])
	}
	return append(line, req.Argv...)
}

// KeepAlive counts as activity, for when the sandbox is busy with something that
// does not go through exec, like traffic to a service running inside it.
func (s *Sandbox) KeepAlive(ctx context.Context) error {
	return s.run(ctx, touchScript, s.activity)
}

// Startup runs a profile's startup script, if it has one, as an ordinary command.
// Call it when a container has started. A script that fails fails the startup,
// and what it printed is the error.
func (s *Sandbox) Startup(ctx context.Context, script string, timeout time.Duration) error {
	if script == "" {
		return nil
	}
	result, err := s.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sh", "-c", script}, Timeout: timeout})
	if err != nil {
		return fmt.Errorf("startup script: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("startup script failed with exit code %d: %s", result.ExitCode, strings.TrimSpace(result.Stderr+"\n"+result.Stdout))
	}
	return nil
}

// run executes a script of ours and turns its exit code into an error.
func (s *Sandbox) run(ctx context.Context, script string, args ...string) error {
	output := &cappedOutput{}
	code, err := s.runner.Run(ctx, Request{Script: script, Args: args, Stdout: output, Stderr: output})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("sandbox operation failed with exit code %d: %s", code, strings.TrimSpace(output.String()))
	}
	return nil
}

// drainLimit totals what all of a command's streams have produced.
type drainLimit struct {
	total    atomic.Int64
	exceeded context.CancelFunc
}

func (l *drainLimit) add(n int) {
	if l.total.Add(int64(n)) > maxDrain {
		l.exceeded()
	}
}

func (l *drainLimit) wasExceeded() bool { return l.total.Load() > maxDrain }

// exitMarker passes a command's stdout through until the marker the script prints
// when the command ends, and takes the exit code from it. The end of the stream
// cannot say this, because a background job holding stdout keeps the stream open.
type exitMarker struct {
	id      string
	out     io.Writer
	onEnd   func()
	mu      sync.Mutex
	pending []byte
	ended   bool
	code    int
}

func newExitMarker(out io.Writer, onEnd func()) *exitMarker {
	return &exitMarker{id: "hastekit-exit-" + uuid.NewString() + ":", out: out, onEnd: onEnd}
}

func (m *exitMarker) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ended {
		return len(p), nil
	}
	m.pending = append(m.pending, p...)
	at := bytes.Index(m.pending, []byte(m.id))
	if at < 0 {
		// Hold back only what could be the start of the marker.
		at = max(len(m.pending)-len(m.id)+1, 0)
	} else if end := bytes.IndexByte(m.pending[at:], ';'); end >= 0 {
		m.code, _ = strconv.Atoi(string(m.pending[at+len(m.id) : at+end]))
		m.ended = true
		m.out.Write(m.pending[:at])
		m.pending = nil
		m.onEnd()
		return len(p), nil
	}
	m.out.Write(m.pending[:at])
	m.pending = append(m.pending[:0], m.pending[at:]...)
	return len(p), nil
}

// flush gives up what was held back, for a stream that ended without a marker.
func (m *exitMarker) flush() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.ended {
		m.out.Write(m.pending)
		m.pending = nil
	}
}

func (m *exitMarker) result() (bool, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ended, m.code
}

// cappedOutput keeps reading past the limit so that a noisy process does not
// block on a full pipe.
type cappedOutput struct {
	mu        sync.Mutex
	data      []byte
	truncated bool
	limit     *drainLimit
}

func (b *cappedOutput) Write(p []byte) (int, error) {
	if b.limit != nil {
		b.limit.add(len(p))
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	left := maxOutput - len(b.data)
	if len(p) > left {
		p = p[:left]
		b.truncated = true
	}
	b.data = append(b.data, p...)
	return n, nil
}

func (b *cappedOutput) note(text string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.data) > 0 {
		b.data = append(b.data, '\n')
	}
	b.data = append(b.data, text...)
}

func (b *cappedOutput) isTruncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

func (b *cappedOutput) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := string(b.data)
	if b.truncated {
		s += "\n[output truncated]"
	}
	return s
}

var _ sandbox.Sandbox = (*Sandbox)(nil)
