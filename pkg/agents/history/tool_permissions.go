package history

import (
	"context"
	"fmt"
	"slices"

	"github.com/bytedance/sonic"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
)

// ToolPermissionsMetaKey is the thread-meta entry the standing tool decisions
// live under. Like the rest of a thread's meta it is written onto each saved
// turn and read back from the newest one, so a decision made on one turn is
// still in force on the next.
const ToolPermissionsMetaKey = "tool_permissions"

// ToolPermissions is a thread's standing answers about individual tools: names
// the user has decided always run, and names the user has decided never do.
// They belong to the conversation rather than to a turn — the point of "don't
// ask me again" is that the next turn doesn't have to carry it.
type ToolPermissions struct {
	AlwaysAllow []string `json:"always_allow,omitempty"`
	AlwaysDeny  []string `json:"always_deny,omitempty"`
}

// Allow records that these tools always run without asking.
func (p *ToolPermissions) Allow(names ...string) {
	for _, name := range names {
		// A name stands on one list or neither. Without taking it off the
		// other, reversing a decision would leave both entries in place and
		// the deny — which wins — would be permanent.
		p.AlwaysDeny = removeName(p.AlwaysDeny, name)
		p.AlwaysAllow = addName(p.AlwaysAllow, name)
	}
}

// Deny records that these tools are refused outright: the run neither executes
// them nor asks the user about them.
func (p *ToolPermissions) Deny(names ...string) {
	for _, name := range names {
		p.AlwaysAllow = removeName(p.AlwaysAllow, name)
		p.AlwaysDeny = addName(p.AlwaysDeny, name)
	}
}

// Clear forgets the standing decision about these tools, putting them back
// under whatever the run's permission mode says.
func (p *ToolPermissions) Clear(names ...string) {
	for _, name := range names {
		p.AlwaysAllow = removeName(p.AlwaysAllow, name)
		p.AlwaysDeny = removeName(p.AlwaysDeny, name)
	}
}

// IsEmpty reports whether the thread has decided nothing about any tool.
func (p ToolPermissions) IsEmpty() bool {
	return len(p.AlwaysAllow) == 0 && len(p.AlwaysDeny) == 0
}

func addName(list []string, name string) []string {
	if slices.Contains(list, name) {
		return list
	}
	return append(list, name)
}

func removeName(list []string, name string) []string {
	return slices.DeleteFunc(list, func(n string) bool { return n == name })
}

// toolPermissionsFromMeta reads the standing decisions out of a thread's meta.
// A malformed entry reads as "nothing decided": permissions nobody can parse
// are permissions nobody granted, and the run's mode is the safe fallback.
func toolPermissionsFromMeta(meta map[string]any) ToolPermissions {
	var permissions ToolPermissions

	raw, ok := meta[ToolPermissionsMetaKey]
	if !ok || raw == nil {
		return permissions
	}

	// The value round-trips through whatever the persistence adapter stores
	// (a map from JSON, or the struct itself when it never left the process),
	// so go through the encoder rather than assuming either shape.
	buf, err := sonic.Marshal(raw)
	if err != nil {
		return ToolPermissions{}
	}
	if err := sonic.Unmarshal(buf, &permissions); err != nil {
		return ToolPermissions{}
	}

	return permissions
}

// ToolPermissions returns the thread's standing tool decisions, as loaded from
// thread meta at the start of the run.
func (cm *ConversationRunManager) ToolPermissions() ToolPermissions {
	return cm.toolPermissions
}

// UpdateToolPermissions changes the thread's standing decisions. The change is
// persisted with the run's next save, and is in force for the rest of this run
// — the tool execution step re-checks the deny list on every call, so denying
// a tool mid-run stops a call the model has already made.
func (cm *ConversationRunManager) UpdateToolPermissions(update func(*ToolPermissions)) {
	update(&cm.toolPermissions)
}

// rememberDecision promotes a one-off answer about a call into a standing
// decision about the tool that call names, when the user asked for it. The
// decision lands on the run manager's permissions, so it persists with this
// run's save and is in force for the rest of the run — a "never do this"
// answer stops the very call it was answering.
//
// A resolution names a call, not a tool, so the tool is resolved from the run
// state holding the pause. If nothing there knows the call — a stale id, or a
// resolution for a run that has moved on — there is no tool to decide about
// and the answer stays a one-off.
func (cm *ConversationRunManager) rememberDecision(res responses.InterruptResolution, allow bool) {
	if !res.RememberAction {
		return
	}

	name := cm.RunState.ToolNameForCall(res.CallID)
	if name == "" {
		return
	}

	// Allow and Deny each take the name off the other list, so the two can
	// never both hold it however often the user changes their mind.
	if allow {
		cm.toolPermissions.Allow(name)
	} else {
		cm.toolPermissions.Deny(name)
	}
}

// ThreadToolPermissions reads a thread's standing tool decisions without
// starting a run — for a UI that shows what the user has decided so far.
func (cm *CommonConversationManager) ThreadToolPermissions(ctx context.Context, namespace, threadID string) (ToolPermissions, error) {
	_, meta, err := cm.lastTurn(ctx, namespace, threadID)
	if err != nil {
		return ToolPermissions{}, err
	}
	return toolPermissionsFromMeta(meta), nil
}

// UpdateThreadToolPermissions applies a standing decision to a thread from
// outside a run — this is where a "don't ask me again" or "never do this"
// answer from the user lands.
//
// The decision is written onto the thread's newest turn, so the persistence
// adapter must treat a save for an existing message id as an update of that
// row's meta (as the bundled adapters do). A thread with no turns yet has
// nothing to write onto and returns an error: permissions follow the
// conversation, and there isn't one until it has been spoken to.
func (cm *CommonConversationManager) UpdateThreadToolPermissions(ctx context.Context, namespace, threadID string, update func(*ToolPermissions)) error {
	last, meta, err := cm.lastTurn(ctx, namespace, threadID)
	if err != nil {
		return err
	}

	permissions := toolPermissionsFromMeta(meta)
	update(&permissions)

	if permissions.IsEmpty() {
		delete(meta, ToolPermissionsMetaKey)
	} else {
		meta[ToolPermissionsMetaKey] = permissions
	}

	return cm.ConversationPersistenceAdapter.SaveMessages(
		ctx, namespace, last.MessageID, "", last.ThreadID, last.ConversationID, nil, meta,
	)
}

// lastTurn returns a thread's newest stored turn and its meta, which is where
// thread-scoped state is kept.
func (cm *CommonConversationManager) lastTurn(ctx context.Context, namespace, threadID string) (ConversationMessage, map[string]any, error) {
	if cm.ConversationPersistenceAdapter == nil {
		return ConversationMessage{}, nil, fmt.Errorf("history: no persistence adapter configured")
	}

	turns, err := cm.ConversationPersistenceAdapter.LoadMessages(ctx, namespace, threadID, "")
	if err != nil {
		return ConversationMessage{}, nil, err
	}
	if len(turns) == 0 {
		return ConversationMessage{}, nil, fmt.Errorf("history: thread %q has no turns to hold permissions", threadID)
	}

	last := turns[len(turns)-1]
	meta := last.Meta
	if meta == nil {
		meta = map[string]any{}
	}
	return last, meta, nil
}
