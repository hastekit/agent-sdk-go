package agents

// ToolAnnotations carries the behavioural hints a tool advertises about
// itself. The shape mirrors MCP's tool annotations so hints coming off an MCP
// server and hints declared on a locally defined function tool are the same
// thing, and a single permission policy can read both.
//
// Every hint is a pointer so "nothing was said" stays distinguishable from
// "false was said": a policy usually wants to treat silence conservatively
// rather than as a promise. The Is* helpers already apply the MCP defaults
// (read-only false, destructive true, idempotent false, open-world true), so
// prefer them over reading the fields directly.
//
// Hints are self-reported. They describe intent, not enforcement — never let a
// hint from an untrusted server widen what a tool is allowed to do.
type ToolAnnotations struct {
	// Title is a human-readable name for the tool, for UI use.
	Title string `json:"title,omitempty"`

	// ReadOnlyHint: the tool does not modify its environment. Default false.
	ReadOnlyHint *bool `json:"readOnlyHint,omitempty"`

	// DestructiveHint: the tool may perform destructive updates, as opposed to
	// only additive ones. Meaningful only when the tool is not read-only.
	// Default true.
	DestructiveHint *bool `json:"destructiveHint,omitempty"`

	// IdempotentHint: repeating the call with the same arguments has no
	// additional effect. Meaningful only when the tool is not read-only.
	// Default false.
	IdempotentHint *bool `json:"idempotentHint,omitempty"`

	// OpenWorldHint: the tool interacts with an open world of external
	// entities (a web search does; a memory lookup does not). Default true.
	OpenWorldHint *bool `json:"openWorldHint,omitempty"`
}

// IsReadOnly reports whether the tool promised not to modify anything.
// Silence is not a promise: an unset hint reads as false.
func (a *ToolAnnotations) IsReadOnly() bool {
	if a == nil || a.ReadOnlyHint == nil {
		return false
	}
	return *a.ReadOnlyHint
}

// IsDestructive reports whether the tool may destroy or overwrite state.
// A read-only tool is never destructive; otherwise an unset hint reads as
// destructive, which is the MCP default and the safe assumption for a
// permission check.
func (a *ToolAnnotations) IsDestructive() bool {
	if a.IsReadOnly() {
		return false
	}
	if a == nil || a.DestructiveHint == nil {
		return true
	}
	return *a.DestructiveHint
}

// IsDeclaredDestructive reports whether the tool actually said it may destroy
// or overwrite state, as opposed to IsDestructive's "assume the worst when
// nothing was said". This is the one a permission check wants: an unannotated
// tool has made no claim, and gating on the absence of a claim would put every
// tool that predates annotations behind a prompt.
func (a *ToolAnnotations) IsDeclaredDestructive() bool {
	if a == nil || a.DestructiveHint == nil {
		return false
	}
	return *a.DestructiveHint && !a.IsReadOnly()
}

// IsIdempotent reports whether repeating the call is harmless. Read-only tools
// are idempotent by definition; otherwise an unset hint reads as false.
func (a *ToolAnnotations) IsIdempotent() bool {
	if a.IsReadOnly() {
		return true
	}
	if a == nil || a.IdempotentHint == nil {
		return false
	}
	return *a.IdempotentHint
}

// IsOpenWorld reports whether the tool reaches outside a closed, known set of
// entities. An unset hint reads as open, the MCP default.
func (a *ToolAnnotations) IsOpenWorld() bool {
	if a == nil || a.OpenWorldHint == nil {
		return true
	}
	return *a.OpenWorldHint
}

// AnnotatedTool is implemented by tools that carry annotations. It is an
// optional interface rather than part of Tool so existing implementations keep
// compiling; reach for AnnotationsOf instead of asserting it by hand.
type AnnotatedTool interface {
	GetAnnotations() *ToolAnnotations
}

// AnnotationsOf returns a tool's annotations, or nil when the tool carries
// none. The nil is usable: every Is* helper is nil-safe and answers with the
// MCP default.
func AnnotationsOf(tool Tool) *ToolAnnotations {
	if at, ok := tool.(AnnotatedTool); ok {
		return at.GetAnnotations()
	}
	return nil
}
