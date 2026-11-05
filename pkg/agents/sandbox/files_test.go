package sandbox_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
)

// outputCap is what providers keep of a command's output, per stream.
const outputCap = 4 << 20

// shellSandbox runs commands in a real shell, and caps their output as providers
// do. Like a provider whose results pass through JSON, it can also rewrite invalid
// UTF-8 in what it returns.
type shellSandbox struct {
	shell, root string
	execs       int
	failAfter   int
	failure     error
	asJSON      bool
}

func (s *shellSandbox) Reference() sandbox.Reference {
	return sandbox.Reference{Provider: "test", ID: "abc"}
}

func (s *shellSandbox) Exec(ctx context.Context, req sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	if s.failure != nil && s.execs >= s.failAfter {
		// Only the cleanup after a failure runs under a fresh context.
		if ctx.Err() == nil && !strings.Contains(req.Argv[2], "rm -f") {
			return nil, s.failure
		}
	}
	s.execs++
	cmd := exec.CommandContext(ctx, s.shell, req.Argv[1:]...)
	cmd.Dir = s.root
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return nil, err
	}
	result := &sandbox.ExecResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: cmd.ProcessState.ExitCode()}
	if len(result.Stdout) > outputCap {
		result.Stdout, result.OutputTruncated = result.Stdout[:outputCap], true
	}
	if s.asJSON {
		result.Stdout = strings.ToValidUTF8(result.Stdout, "�")
	}
	return result, nil
}

func eachShell(t *testing.T, test func(t *testing.T, s *shellSandbox, outside string)) {
	t.Helper()
	for _, shell := range []string{"sh", "dash", "bash"} {
		path, err := exec.LookPath(shell)
		if err != nil {
			continue
		}
		t.Run(shell, func(t *testing.T) {
			t.Parallel()
			base, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			root, outside := filepath.Join(base, "workspace"), filepath.Join(base, "outside")
			for _, dir := range []string{root, outside} {
				if err := os.Mkdir(dir, 0755); err != nil {
					t.Fatal(err)
				}
			}
			test(t, &shellSandbox{shell: path, root: root}, outside)
		})
	}
}

func TestFilesRoundTripAnyContent(t *testing.T) {
	eachShell(t, func(t *testing.T, s *shellSandbox, _ string) {
		ctx := context.Background()
		for name, content := range map[string][]byte{
			"empty":                nil,
			"text.txt":             []byte("héllo\n"),
			"binary.bin":           {0, 255, 128, '\n', '\'', '"', '$', '`', '\\'},
			"nested/dir/a b's.txt": []byte("spaces and quotes in the name"),
			"chunks.bin":           bytes.Repeat([]byte{0, 1, 2, 254, 255}, 60<<10),
		} {
			if err := sandbox.WriteFile(ctx, s, name, content); err != nil {
				t.Fatal(name, err)
			}
			got, err := sandbox.ReadFile(ctx, s, filepath.Join(s.root, name))
			if err != nil {
				t.Fatal(name, err)
			}
			if !bytes.Equal(got, content) {
				t.Fatalf("%s: got %d bytes, want %d", name, len(got), len(content))
			}
		}
		entries, err := os.ReadDir(filepath.Join(s.root, "nested/dir"))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Fatalf("left behind: %v", entries)
		}
	})
}

// Base64 is what lets the bytes survive a provider that passes through JSON, so a
// binary file can still be recognised as binary.
func TestReadFileSurvivesATextOnlyTransport(t *testing.T) {
	eachShell(t, func(t *testing.T, s *shellSandbox, _ string) {
		s.asJSON = true
		content := []byte{0, 255, 128, 'x'}
		if err := os.WriteFile(filepath.Join(s.root, "binary"), content, 0644); err != nil {
			t.Fatal(err)
		}
		got, err := sandbox.ReadFile(context.Background(), s, "binary")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, content) || utf8.Valid(got) {
			t.Fatalf("got %v", got)
		}
	})
}

func TestReadFileLimits(t *testing.T) {
	eachShell(t, func(t *testing.T, s *shellSandbox, _ string) {
		ctx := context.Background()
		if err := os.WriteFile(filepath.Join(s.root, "fits"), make([]byte, sandbox.MaxReadFile), 0644); err != nil {
			t.Fatal(err)
		}
		if got, err := sandbox.ReadFile(ctx, s, "fits"); err != nil || len(got) != sandbox.MaxReadFile {
			t.Fatalf("%d bytes: %v", len(got), err)
		}
		if err := os.WriteFile(filepath.Join(s.root, "over"), make([]byte, sandbox.MaxReadFile+1), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := sandbox.ReadFile(ctx, s, "over"); !errors.Is(err, sandbox.ErrFileTooLarge) {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestReadFileFailures(t *testing.T) {
	eachShell(t, func(t *testing.T, s *shellSandbox, _ string) {
		ctx := context.Background()
		if _, err := sandbox.ReadFile(ctx, s, "missing"); !errors.Is(err, sandbox.ErrFileNotFound) {
			t.Fatalf("err=%v", err)
		}
		if err := os.Mkdir(filepath.Join(s.root, "dir"), 0755); err != nil {
			t.Fatal(err)
		}
		if _, err := sandbox.ReadFile(ctx, s, "dir"); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err=%v", err)
		}
		for _, name := range []string{"", " ", ".", "dir/.."} {
			if _, err := sandbox.ReadFile(ctx, s, name); err == nil {
				t.Fatalf("read %q", name)
			}
		}
	})
}

func TestInterruptedWriteLeavesTheOldFileAlone(t *testing.T) {
	eachShell(t, func(t *testing.T, s *shellSandbox, _ string) {
		target := filepath.Join(s.root, "report.txt")
		if err := os.WriteFile(target, []byte("original"), 0644); err != nil {
			t.Fatal(err)
		}
		// The first chunk lands, and the connection goes before the second.
		broken := errors.New("connection lost")
		s.failure, s.failAfter = broken, 1
		err := sandbox.WriteFile(context.Background(), s, "report.txt", make([]byte, 100<<10))
		if !errors.Is(err, broken) {
			t.Fatalf("err=%v", err)
		}
		if got, _ := os.ReadFile(target); string(got) != "original" {
			t.Fatalf("target is %q", got)
		}
		entries, err := os.ReadDir(s.root)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Fatalf("left behind: %v", entries)
		}
	})
}

func TestWriteFileRefusesToReplaceADirectory(t *testing.T) {
	eachShell(t, func(t *testing.T, s *shellSandbox, _ string) {
		if err := os.Mkdir(filepath.Join(s.root, "dir"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := sandbox.WriteFile(context.Background(), s, "dir", []byte("x")); err == nil {
			t.Fatal("replaced a directory")
		}
	})
}

func TestRemoveFile(t *testing.T) {
	eachShell(t, func(t *testing.T, s *shellSandbox, _ string) {
		ctx := context.Background()
		if err := os.MkdirAll(filepath.Join(s.root, "full/inner"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(s.root, "empty"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.root, "file"), nil, 0644); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"file", "empty"} {
			if err := sandbox.RemoveFile(ctx, s, name); err != nil {
				t.Fatal(name, err)
			}
			if _, err := os.Lstat(filepath.Join(s.root, name)); err == nil {
				t.Fatal(name, "survived")
			}
		}
		for _, name := range []string{"file", "no/such/parent"} {
			if err := sandbox.RemoveFile(ctx, s, name); !errors.Is(err, sandbox.ErrFileNotFound) {
				t.Fatalf("%s: err=%v", name, err)
			}
		}
		if err := sandbox.RemoveFile(ctx, s, "full"); err == nil {
			t.Fatal("removed a directory with contents")
		}
		if err := sandbox.RemoveFile(ctx, s, s.root); err == nil {
			t.Fatal("removed the workspace")
		}
	})
}

func TestFilesStayInsideTheWorkspace(t *testing.T) {
	eachShell(t, func(t *testing.T, s *shellSandbox, outside string) {
		ctx := context.Background()
		secret := filepath.Join(outside, "secret")
		if err := os.WriteFile(secret, []byte("secret"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(s.root, "link")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(secret, filepath.Join(s.root, "filelink")); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"../outside/secret", secret, "link/secret", "filelink"} {
			if _, err := sandbox.ReadFile(ctx, s, name); err == nil {
				t.Errorf("read %s", name)
			}
		}
		for _, name := range []string{"../outside/new", filepath.Join(outside, "new"), "link/new", "link/deep/er/new"} {
			if err := sandbox.WriteFile(ctx, s, name, []byte("x")); err == nil {
				t.Errorf("wrote %s", name)
			}
		}
		for _, name := range []string{"link/secret", "../outside/secret", secret} {
			if err := sandbox.RemoveFile(ctx, s, name); err == nil {
				t.Errorf("removed %s", name)
			}
		}
		entries, err := os.ReadDir(outside)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Fatalf("outside changed: %v", entries)
		}
		// The link itself is in the workspace, so it can be replaced or removed.
		if err := sandbox.WriteFile(ctx, s, "filelink", []byte("replaced")); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(secret); string(got) != "secret" {
			t.Fatalf("wrote through a link: %q", got)
		}
		if err := sandbox.RemoveFile(ctx, s, "link"); err != nil {
			t.Fatal(err)
		}
	})
}
