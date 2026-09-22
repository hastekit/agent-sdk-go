package tools

import (
	"bytes"
	"context"
	"io"
	"maps"
	"strings"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// This implementation needs only the common execution and file contract.
type portableSandbox struct {
	argv    []string
	data    []byte
	workdir string
	execErr error
}

func (s *portableSandbox) Reference() sandbox.Reference {
	return sandbox.Reference{Provider: "test", ID: "portable"}
}
func (s *portableSandbox) Exec(_ context.Context, req sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	if s.execErr != nil {
		return nil, s.execErr
	}
	s.argv = req.Argv
	s.workdir = req.Workdir
	return &sandbox.ExecResult{Stdout: "portable"}, nil
}
func (s *portableSandbox) Files() sandbox.FileSystem { return s }
func (s *portableSandbox) Read(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.data)), nil
}
func (s *portableSandbox) Write(_ context.Context, _ string, r io.Reader) error {
	var err error
	s.data, err = io.ReadAll(r)
	return err
}
func (s *portableSandbox) Remove(context.Context, string) error { s.data = nil; return nil }

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
	m := &portableProvider{sb: &portableSandbox{}}
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
	call.Arguments = `{"file_path":"a.txt","content":"hello"}`
	if _, err := NewWriteFileTool(m, "base", nil).Execute(ctx, call); err != nil {
		t.Fatal(err)
	}
	call.Arguments = `{"file_path":"a.txt"}`
	if _, err := NewReadFileTool(m, "base", nil).Execute(ctx, call); err != nil {
		t.Fatal(err)
	}
	m.sb.data = []byte{255, 0}
	if result, err := NewReadFileTool(m, "base", nil).Execute(ctx, call); err != nil || !strings.Contains(*result.Output.OfString, "file is binary") {
		t.Fatal("binary bytes exposed as text", err)
	}
	if _, err := NewDeleteFileTool(m, "base", nil).Execute(ctx, call); err != nil {
		t.Fatal(err)
	}
}

type denySandbox struct{}

func (denySandbox) WrapToolCall(agents.ToolCallFunc) agents.ToolCallFunc {
	return func(_ context.Context, _ *agents.BaseTool, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
		return agents.ToolCallResult(call, "denied"), nil
	}
}
func TestDeniedSandboxToolDoesNotProvision(t *testing.T) {
	backend := &portableProvider{sb: &portableSandbox{}}
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
	if string(backend.sb.data) != "ok" {
		t.Fatal("next tool did not run")
	}
	if calls[1].ToolCall.State != nil {
		t.Fatal("executor mutated caller snapshot")
	}
}
