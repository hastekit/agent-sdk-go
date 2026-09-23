package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
)

func TestV2SDKBinaryAndExec(t *testing.T) {
	root := t.TempDir()
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/files", func(w http.ResponseWriter, r *http.Request) { handleFilesV2(w, r, root) })
	mux.HandleFunc("/v2/exec", func(w http.ResponseWriter, r *http.Request) { handleExecV2(w, r, root) })
	server := httptest.NewServer(mux)
	defer server.Close()
	s, err := sandbox.NewDaemonSandbox(sandbox.DaemonConfig{Reference: sandbox.Reference{Provider: "hastekit", ID: "test"}, Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	payload := []byte{0, 255, 128, 10}
	name := "nested/a b.bin"
	if err := s.Files().Write(ctx, name, bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	r, err := s.Files().Read(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	r.Close()
	if !bytes.Equal(b, payload) {
		t.Fatal(b)
	}
	result, err := s.Exec(ctx, sandbox.ExecRequest{Argv: []string{"printf", "%s", "$(touch injected); 'hello'"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "$(touch injected); 'hello'" || result.ExitCode != 0 {
		t.Fatal(result)
	}
	if _, err := os.Stat(filepath.Join(root, "injected")); !os.IsNotExist(err) {
		t.Fatal("shell interpolation occurred")
	}
	result, err = s.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sh", "-c", "printf out; printf err >&2; exit 7"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "out" || result.Stderr != "err" || result.ExitCode != 7 {
		t.Fatal(result)
	}
	if err := s.Files().Remove(ctx, name); err != nil {
		t.Fatal(err)
	}
}
func TestV2FilesConfinedAndCancelledWriteAtomic(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0600)
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, "/v2/files?path="+url.QueryEscape("link/secret"), strings.NewReader("overwrite"))
		handleFilesV2(w, r, root)
		if w.Code < 400 {
			t.Fatalf("%s escaped: %d", method, w.Code)
		}
	}
	os.WriteFile(filepath.Join(root, "good"), []byte("original"), 0600)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("PUT", "/v2/files?path=good", strings.NewReader("partial")).WithContext(ctx)
	handleFilesV2(w, r, root)
	b, _ := os.ReadFile(filepath.Join(root, "good"))
	if string(b) != "original" {
		t.Fatal("cancelled upload replaced file")
	}
	matches, _ := filepath.Glob(filepath.Join(root, ".upload-*"))
	if len(matches) != 0 {
		t.Fatal("temporary upload leaked")
	}
}
func TestV2TimeoutKillsProcessGroup(t *testing.T) {
	req := sandbox.ExecRequest{Argv: []string{"sh", "-c", "sleep 30 & wait"}, Timeout: 50 * time.Millisecond}
	b, _ := json.Marshal(req)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v2/exec", bytes.NewReader(b))
	start := time.Now()
	handleExecV2(w, r, t.TempDir())
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var result sandbox.ExecResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 124 || time.Since(start) > 3*time.Second {
		t.Fatalf("timeout failed: %+v %v", result, time.Since(start))
	}
}
