package tools

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// This implementation needs only Exec, which it runs in a real shell, because the
// file tools are built on it.
type portableSandbox struct {
	root    string
	argv    []string
	workdir string
	execErr error
}

func (s *portableSandbox) Reference() sandbox.Reference {
	return sandbox.Reference{Provider: "test", ID: "portable"}
}
func (s *portableSandbox) Exec(ctx context.Context, req sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	s.argv = req.Argv
	s.workdir = req.Workdir
	if s.execErr != nil {
		return nil, s.execErr
	}
	cmd := exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
	cmd.Dir = s.root
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return nil, err
	}
	return &sandbox.ExecResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: cmd.ProcessState.ExitCode()}, nil
}

func newPortableSandbox(t *testing.T) *portableSandbox {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &portableSandbox{root: root}
}

type portableProvider struct {
	sb      *portableSandbox
	key     sandbox.SessionKey
	creates int
}

func (m *portableProvider) Create(_ context.Context, req sandbox.CreateRequest) (sandbox.Sandbox, error) {
	m.creates++
	m.key = req.Session
	return m.sb, nil
}
func (m *portableProvider) Connect(context.Context, sandbox.Reference) (sandbox.Sandbox, error) {
	return m.sb, nil
}
func (m *portableProvider) Delete(context.Context, sandbox.Reference) error { return nil }
func TestSandboxToolsUseGenericInterface(t *testing.T) {
	m := &portableProvider{sb: newPortableSandbox(t)}
	ctx := context.Background()
	call := &agents.ToolCall{Namespace: "project", SessionID: "session", FunctionCallMessage: &responses.FunctionCallMessage{}}
	call.Arguments = `{"code":"printf hello","workdir":"project"}`
	call.State = map[string]string{"sandbox_cwd": "obsolete-directory"}
	result, err := NewBashTool(m, "base", nil).Execute(ctx, call)
	if err != nil {
		t.Fatal(err)
	}
	maps.Copy(call.State, result.StateUpdates)
	if call.State[sandbox.StateKey] == "" {
		t.Fatal("reference not mirrored")
	}

	if m.key != (sandbox.SessionKey{Namespace: "project", SessionID: "session"}) || len(m.sb.argv) != 3 || m.sb.argv[0] != "bash" {
		t.Fatal("incorrect portable execution")
	}
	if m.sb.workdir != "project" {
		t.Fatal("explicit workdir not honored")
	}
	call.Arguments = `{"code":"pwd"}`
	result, err = NewBashTool(m, "base", nil).Execute(ctx, call)
	if err != nil {
		t.Fatal(err)
	}
	if m.sb.workdir != "" || result.StateUpdates["sandbox_cwd"] != "" {
		t.Fatal("shell cwd persisted across calls")
	}
	output := func(tool agents.Tool, arguments string) string {
		t.Helper()
		call.Arguments = arguments
		result, err := tool.Execute(ctx, call)
		if err != nil {
			t.Fatal(err)
		}
		return *result.Output.OfString
	}
	if got := output(NewWriteFileTool(m, "base", nil), `{"file_path":"a.txt","content":"héllo 'world'\n"}`); !strings.Contains(got, "Written successfully") {
		t.Fatal(got)
	}
	var read ReadFileToolOutput
	if err := sonic.Unmarshal([]byte(output(NewReadFileTool(m, "base", nil), `{"file_path":"a.txt"}`)), &read); err != nil {
		t.Fatal(err)
	}
	if read.Content != "héllo 'world'\n" || read.Path != "a.txt" {
		t.Fatalf("%+v", read)
	}
	if err := os.WriteFile(filepath.Join(m.sb.root, "a.bin"), []byte{255, 0}, 0644); err != nil {
		t.Fatal(err)
	}
	if got := output(NewReadFileTool(m, "base", nil), `{"file_path":"a.bin"}`); !strings.Contains(got, "file is binary") {
		t.Fatal("binary bytes exposed as text", got)
	}
	if err := os.WriteFile(filepath.Join(m.sb.root, "big"), make([]byte, sandbox.MaxReadFile+1), 0644); err != nil {
		t.Fatal(err)
	}
	if got := output(NewReadFileTool(m, "base", nil), `{"file_path":"big"}`); !strings.Contains(got, "exceeds 3 MB") {
		t.Fatal(got)
	}
	if got := output(NewReadFileTool(m, "base", nil), `{"file_path":"missing"}`); !strings.Contains(got, "file not found") {
		t.Fatal(got)
	}
	if got := output(NewDeleteFileTool(m, "base", nil), `{"file_path":"a.txt"}`); got != "Deleted successfully" {
		t.Fatal(got)
	}
	if _, err := os.Stat(filepath.Join(m.sb.root, "a.txt")); err == nil {
		t.Fatal("file survived delete")
	}
	if got := output(NewDeleteFileTool(m, "base", nil), `{"file_path":"a.txt"}`); !strings.Contains(got, "file not found") {
		t.Fatal(got)
	}
}

type denySandbox struct{}

func (denySandbox) WrapToolCall(agents.ToolCallFunc) agents.ToolCallFunc {
	return func(_ context.Context, _ *agents.BaseTool, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
		return agents.ToolCallResult(call, "denied"), nil
	}
}
func TestDeniedSandboxToolDoesNotProvision(t *testing.T) {
	backend := &portableProvider{sb: newPortableSandbox(t)}
	tool := NewBashTool(backend, "base", nil)
	call := &agents.ToolCall{Namespace: "ns", SessionID: "session", FunctionCallMessage: &responses.FunctionCallMessage{Name: "execute_bash_commands", Arguments: `{"code":"true"}`}}
	executor := &agents.DefaultToolExecutor{Middlewares: []agents.ToolCallMiddleware{denySandbox{}}}
	results := executor.ExecuteAll(context.Background(), []agents.ExecutableToolCall{{Tool: tool, ToolCall: call, ToolName: call.Name}})
	if results[0].Err != nil {
		t.Fatal(results[0].Err)
	}
	if backend.key.SessionID != "" {
		t.Fatal("denied tool provisioned a sandbox")
	}
}

func TestSequentialSandboxToolsRetainReferenceAfterOperationFailure(t *testing.T) {
	backend := &portableProvider{sb: &portableSandbox{execErr: context.DeadlineExceeded}}
	calls := []agents.ExecutableToolCall{
		{Tool: NewBashTool(backend, "base", nil), ToolCall: &agents.ToolCall{Namespace: "ns", SessionID: "session", FunctionCallMessage: &responses.FunctionCallMessage{Name: "execute_bash_commands", Arguments: `{"code":"true"}`}}},
		{Tool: NewWriteFileTool(backend, "base", nil), ToolCall: &agents.ToolCall{Namespace: "ns", SessionID: "session", FunctionCallMessage: &responses.FunctionCallMessage{Name: "write_file", Arguments: `{"file_path":"test","content":"ok"}`}}},
	}
	results := (&agents.DefaultToolExecutor{}).ExecuteAll(context.Background(), calls)
	for _, result := range results {
		if result.Err != nil {
			t.Fatal(result.Err)
		}
	}
	if backend.creates != 1 {
		t.Fatalf("created %d sandboxes", backend.creates)
	}
	if !strings.Contains(*results[0].Response.Output.OfString, "context deadline exceeded") {
		t.Fatal("failure not reported")
	}
	if results[0].Response.StateUpdates[sandbox.StateKey] == "" {
		t.Fatal("reference lost on operation failure")
	}
	if !strings.Contains(*results[1].Response.Output.OfString, "context deadline exceeded") || backend.sb.argv[0] != "sh" {
		t.Fatal("next tool did not run")
	}
	if calls[1].ToolCall.State != nil {
		t.Fatal("executor mutated caller snapshot")
	}
}
