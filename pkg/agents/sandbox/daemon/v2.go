package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
)

// cappedOutput continues draining after the limit so a noisy process cannot
// deadlock on a full pipe or consume unbounded daemon memory.
type cappedOutput struct {
	mu        sync.Mutex
	data      []byte
	truncated bool
}

func (b *cappedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	left := (4 << 20) - len(b.data)
	if len(p) > left {
		p = p[:left]
		b.truncated = true
	}
	b.data = append(b.data, p...)
	return n, nil
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

func handleExecV2(w http.ResponseWriter, r *http.Request, root string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req sandbox.ExecRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	timeout, err := sandbox.ValidateExec(req)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	workdir, err := resolvePath(root, req.Workdir)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
	cmd.Dir = workdir
	cmd.Env = os.Environ()
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	stdout, stderr := &cappedOutput{}, &cappedOutput{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	start := time.Now()
	err = cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if ctx.Err() != nil {
			code = 124
		} else if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			http.Error(w, err.Error(), 500)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sandbox.ExecResult{OutputTruncated: stdout.truncated || stderr.truncated, Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: code, DurationMilli: time.Since(start).Milliseconds()})
}

// Files are confined with os.Root, including symlinks. PUT publishes atomically,
// so cancelled uploads never replace a valid file with a partial one.
func handleFilesV2(w http.ResponseWriter, r *http.Request, root string) {
	name := r.URL.Query().Get("path")
	if name == "" {
		http.Error(w, "path is required", 400)
		return
	}
	absolute, err := resolvePath(root, name)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	rel, err := filepath.Rel(filepath.Clean(root), absolute)
	if err != nil || rel == "." {
		http.Error(w, "a file path is required", 400)
		return
	}
	fs, err := os.OpenRoot(root)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer fs.Close()
	fail := func(err error) {
		status := 500
		if os.IsNotExist(err) {
			status = 404
		}
		if errors.Is(err, os.ErrPermission) || strings.Contains(err.Error(), "escapes") {
			status = 403
		}
		http.Error(w, err.Error(), status)
	}
	switch r.Method {
	case http.MethodGet:
		f, err := fs.Open(rel)
		if err != nil {
			fail(err)
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			fail(err)
			return
		}
		if !info.Mode().IsRegular() {
			http.Error(w, "not a regular file", 400)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprint(info.Size()))
		_, _ = io.Copy(w, f)
	case http.MethodPut:
		if err := fs.MkdirAll(filepath.Dir(rel), 0755); err != nil {
			fail(err)
			return
		}
		tmp := filepath.Join(filepath.Dir(rel), ".upload-"+uuid.NewString())
		f, err := fs.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			fail(err)
			return
		}
		defer fs.Remove(tmp)
		_, err = io.Copy(f, r.Body)
		if err == nil {
			err = r.Context().Err()
		}
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			fail(err)
			return
		}
		if err = fs.Rename(tmp, rel); err != nil {
			fail(err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		if err := fs.Remove(rel); err != nil {
			fail(err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", 405)
	}
}
