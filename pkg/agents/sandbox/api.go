package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

var (
	ErrNotFound    = errors.New("sandbox not found")
	ErrUnsupported = errors.New("sandbox capability unsupported")
	ErrConflict    = errors.New("sandbox conflict")
)

// Reference identifies a provider resource, without credentials or network topology.
type Reference struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
}

type SessionKey struct {
	Namespace string `json:"namespace"`
	SessionID string `json:"session_id"`
}

// CreateRequest selects a provider-configured profile. Zero TTL uses the profile
// default. Session and AgentName are application metadata, not provider IDs.
type CreateRequest struct {
	Profile   string            `json:"profile"`
	Env       map[string]string `json:"env,omitempty"`
	TTL       time.Duration     `json:"ttl,omitempty"`
	Session   SessionKey        `json:"session"`
	AgentName string            `json:"agent_name,omitempty"`
}

// Provider owns transport, authentication, readiness, and provider-specific configuration.
// Connect must never silently replace a missing sandbox with an empty environment.
type Provider interface {
	Create(context.Context, CreateRequest) (Sandbox, error)
	Connect(context.Context, Reference) (Sandbox, error)
	Delete(context.Context, Reference) error
}

type Sandbox interface {
	Reference() Reference
	Exec(context.Context, ExecRequest) (*ExecResult, error)
	Files() FileSystem
}

// ExecRequest runs an argument vector without implicit shell interpolation.
// Use []string{"sh", "-c", script} when shell syntax is intended. Zero Timeout
// selects DefaultExecTimeout. Context cancellation stops waiting; it does not
// universally guarantee remote termination. Commands must not be retried blindly.
type ExecRequest struct {
	Argv    []string          `json:"argv"`
	Workdir string            `json:"workdir,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Timeout time.Duration     `json:"timeout,omitempty"`
}

const DefaultExecTimeout = 60 * time.Second

type ExecResult struct {
	OutputTruncated bool `json:"output_truncated,omitempty"`
	// OutputCombined means the provider returns both streams in Stdout.
	OutputCombined bool   `json:"output_combined,omitempty"`
	Stdout         string `json:"stdout"`
	Stderr         string `json:"stderr"`
	ExitCode       int    `json:"exit_code"`
	DurationMilli  int64  `json:"duration_ms"`
}

// FileSystem transfers bytes, not model-visible text. Callers close returned readers.
// Relative paths resolve against the provider's workspace. Write creates parents.
type FileSystem interface {
	Read(context.Context, string) (io.ReadCloser, error)
	Write(context.Context, string, io.Reader) error
	Remove(context.Context, string) error
}

// EndpointResolver is optional access to a service running inside the sandbox.
type EndpointResolver interface {
	Endpoint(context.Context, int) (string, error)
}

// ValidateExec is shared by provider adapters; it returns the effective timeout.
func ValidateExec(req ExecRequest) (time.Duration, error) {
	if len(req.Argv) == 0 || req.Argv[0] == "" {
		return 0, fmt.Errorf("sandbox exec: executable is required")
	}
	for _, arg := range req.Argv {
		if strings.ContainsRune(arg, 0) {
			return 0, fmt.Errorf("sandbox exec: NUL in argument")
		}
	}
	if req.Timeout < 0 {
		return 0, fmt.Errorf("sandbox exec: negative timeout")
	}
	if strings.ContainsRune(req.Workdir, 0) {
		return 0, fmt.Errorf("sandbox exec: NUL in workdir")
	}
	for k, v := range req.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) {
			return 0, fmt.Errorf("sandbox exec: invalid environment")
		}
	}
	if req.Timeout == 0 {
		return DefaultExecTimeout, nil
	}
	return req.Timeout, nil
}

// QuoteArgv encodes literal arguments for providers whose API accepts shell text.
func QuoteArgv(argv []string) string {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'"
	}
	return strings.Join(quoted, " ")
}
