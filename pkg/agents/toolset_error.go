package agents

import (
	"errors"
	"strings"
)

// A toolset that cannot be listed no longer fails the run. Its tools are simply
// absent, and the agent is told so in its prompt: a model that knows a server
// is unreachable says so, where one that just finds a tool missing invents an
// explanation, or answers as if it had called it.

// ToolsetErrorKind says why a toolset could not be listed. It decides the
// wording the model is given, because the two ask different things of the user:
// a server that needs reconnecting is something they can act on, one that is
// merely down is not.
type ToolsetErrorKind string

const (
	// ToolsetErrorAuth is a credential the server rejected — a 401 or 403.
	// Retrying changes nothing; the user has to authorize.
	ToolsetErrorAuth ToolsetErrorKind = "auth"

	// ToolsetErrorUnavailable is everything else: the server is down,
	// unreachable, or answered with something unusable.
	ToolsetErrorUnavailable ToolsetErrorKind = "unavailable"
)

// ToolsetError is a listing failure the toolset has classified for itself.
// Only the toolset knows what its transport's errors mean, so it says; a
// toolset that returns a bare error is read as ToolsetErrorUnavailable.
type ToolsetError struct {
	Kind ToolsetErrorKind
	Err  error
}

func NewToolsetError(kind ToolsetErrorKind, err error) *ToolsetError {
	return &ToolsetError{Kind: kind, Err: err}
}

func (e *ToolsetError) Error() string {
	if e.Err == nil {
		return string(e.Kind)
	}
	return e.Err.Error()
}

func (e *ToolsetError) Unwrap() error { return e.Err }

// ConnectorStatus is how one MCP server fared this run: whether it connected,
// how many tools it contributed, and — when it did not — why.
//
// Every configured connector gets one, not just the ones that failed. What the
// agent has is as much a fact about the run as what it is missing, and a model
// told a connector holds twelve tools stops guessing at a thirteenth.
//
// Plain data rather than an error because it travels: into the prompt's
// Dependencies, and across a durable runtime's serialization boundary with it.
type ConnectorStatus struct {
	Name      string `json:"name"`
	Connected bool   `json:"connected"`

	// ToolCount is what this connector contributed after filtering, so it is
	// the number the model can actually call, not the number the server
	// publishes.
	ToolCount int `json:"tool_count"`

	// Kind and Detail are set only when Connected is false. They are the
	// classification and the raw cause; turning either into something a model
	// reads is the prompt's job, not this package's.
	Kind   ToolsetErrorKind `json:"kind,omitempty"`
	Detail string           `json:"detail,omitempty"`
}

// connectedStatus records a connector that listed, and what it brought.
func connectedStatus(name string, toolCount int) ConnectorStatus {
	return ConnectorStatus{Name: connectorName(name), Connected: true, ToolCount: toolCount}
}

// failedStatus records a connector that did not list, and why.
func failedStatus(name string, err error) ConnectorStatus {
	c := ConnectorStatus{Name: connectorName(name), Kind: classifyToolsetError(err)}
	// An auth failure's text is only ever "Unauthorized" — the kind already
	// says that, and better.
	if c.Kind != ToolsetErrorAuth && err != nil {
		c.Detail = truncateToolsetDetail(err.Error())
	}
	return c
}

// connectorName stands in for a toolset that reports no name of its own, so a
// status is never anonymous in the prompt.
func connectorName(name string) string {
	if name == "" {
		return "an MCP server"
	}
	return name
}

// toolsetDetailMax caps what of an error's text reaches the prompt. The text
// is a transport's, not ours, and an unbounded one would be spending the
// context window on a stack of URLs.
const toolsetDetailMax = 160

// classifyToolsetError takes the toolset's own classification. Only the toolset
// can make one: the kind comes from the HTTP status the server answered with,
// which is known at the transport and nowhere above it.
//
// A toolset reached across a durable runtime's boundary has to carry its
// classification over in that runtime's own terms, because a Go error does not
// survive the crossing — see toolsetListError in each of them.
func classifyToolsetError(err error) ToolsetErrorKind {
	var te *ToolsetError
	if errors.As(err, &te) && te.Kind != "" {
		return te.Kind
	}
	return ToolsetErrorUnavailable
}

func truncateToolsetDetail(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= toolsetDetailMax {
		return s
	}
	return s[:toolsetDetailMax] + "…"
}
