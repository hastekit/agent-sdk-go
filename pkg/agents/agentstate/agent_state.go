package agentstate

import (
	"encoding/json"
	"sort"

	"github.com/bytedance/sonic"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/messages"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
)

// Step represents the current step in the agent execution state machine
type Step string

const (
	StepCallLLM       Step = "call_llm"
	StepExecuteTools  Step = "execute_tools"
	StepAwaitApproval Step = "await_approval"
	StepComplete      Step = "complete"
)

// RunStatus represents the overall status of a run
type RunStatus string

const (
	RunStatusInProgress RunStatus = "running"
	RunStatusPaused     RunStatus = "paused"
	RunStatusCompleted  RunStatus = "completed"
	RunStatusError      RunStatus = "error"
)

// RunState encapsulates the execution state of an agent run
type RunState struct {
	CurrentStep   Step            `json:"current_step"`
	LoopIteration int             `json:"loop_iteration"`
	Usage         responses.Usage `json:"usage"`
	// ContextTokens is the measured half of the context-occupancy signal: the
	// most recent call's reported total, being the prompt it was given plus the
	// reply it produced. Both are in the next prompt, and both are numbers the
	// provider supplied — nothing here is inferred.
	ContextTokens int `json:"context_tokens"`

	// PendingContextTokens is the estimated half: everything appended since
	// that call — tool results, user turns, queued messages — which no usage
	// report has covered yet. It holds estimates only, and is cleared the
	// moment the next call reports a real number that supersedes it.
	//
	// The summarizer triggers on the sum. See ConversationRunManager.
	PendingContextTokens int `json:"pending_context_tokens"`

	QueuedApprovals  []string           `json:"queued_approvals,omitempty"`
	QueuedRejections []string           `json:"queued_rejections,omitempty"`
	QueuedMessages   []messages.Message `json:"queued_messages,omitempty"`
	TraceID          string             `json:"traceid"`

	// PendingTools is a work list of tools to be executed immediately
	PendingToolCalls []responses.FunctionCallMessage `json:"pending_tool_calls,omitempty"`

	// ToolsAwaitingApproval list of tools that is awaiting to raise approval interrupt
	ToolsAwaitingApproval []responses.FunctionCallMessage `json:"tools_awaiting_approval,omitempty"`

	// PendingNestedToolCalls maps the parent tool call ID to the nested tool call ID
	PendingNestedToolCalls map[string]string `json:"pending_nested_tool_calls,omitempty"`

	// PausedToolCalls maps the parent tool call ID to the paused tool call
	PausedToolCalls map[string]responses.FunctionCallMessage `json:"paused_tool_calls,omitempty"`

	// Interrupts maps a paused call ID to its flexible Interrupt
	Interrupts map[string]responses.Interrupt `json:"interrupts,omitempty"`

	// Resolutions holds data-carrying interrupt resolutions (those with
	// Content, e.g. a submitted form) drained from the incoming resume
	// message, keyed by resolved call id. It's transient — populated on the
	// resume invocation and consumed the same invocation when building the
	// resuming tool's ResumeMessages — so it isn't persisted (no ToMeta
	// entry, json:"-").
	Resolutions map[string]responses.InterruptResolution `json:"-"`

	// LastAgentName is the name of the agent that was responding to the user last.
	LastAgentName string `json:"last_agent_name,omitempty"`
}

// PendingInterrupts projects the current PendingToolCalls into the unified
// Interrupt view. Calls that raised an explicit Interrupt (workflow
// approval, URL elicitation, …) return their recorded record with mode +
// payload; any remaining paused call is a tool-level RequiresApproval gate
// and is synthesized as an approval-mode interrupt. Every pause therefore
// surfaces through this single list.
func (s *RunState) PendingInterrupts() []responses.Interrupt {
	if len(s.PendingToolCalls) == 0 {
		return nil
	}
	out := make([]responses.Interrupt, 0, len(s.PendingToolCalls))
	for _, tc := range s.PendingToolCalls {
		if intr, ok := s.Interrupts[tc.CallID]; ok {
			out = append(out, intr)
			continue
		}
		out = append(out, responses.Interrupt{
			FunctionCallMessage: tc,
			Mode:                responses.InterruptModeApproval,
		})
	}
	return out
}

// ToolNameForCall resolves the tool a paused call was going to run. A
// resolution names only the call it answers, so this is how an answer about
// one call becomes a decision about a tool. Returns "" when the id belongs to
// no call this run is holding.
func (s *RunState) ToolNameForCall(callID string) string {
	if intr, ok := s.Interrupts[callID]; ok {
		return intr.FunctionCallMessage.Name
	}
	if tc, ok := s.PausedToolCalls[callID]; ok {
		return tc.Name
	}
	for _, tc := range s.PendingToolCalls {
		if tc.CallID == callID {
			return tc.Name
		}
	}
	for _, tc := range s.ToolsAwaitingApproval {
		if tc.CallID == callID {
			return tc.Name
		}
	}
	return ""
}

// NextStep returns what the agent should do next
func (s *RunState) NextStep() Step {
	return s.CurrentStep
}

// TransitionToLLM moves to the LLM call step and increments loop iteration
func (s *RunState) TransitionToLLM() {
	s.CurrentStep = StepCallLLM
	s.LoopIteration++
}

// TransitionToExecuteTools moves to tool execution step with the given tools
func (s *RunState) TransitionToExecuteTools(tools []responses.FunctionCallMessage) {
	s.CurrentStep = StepExecuteTools
	s.PendingToolCalls = tools
}

// TransitionToAwaitApproval moves to await approval step with the given tools
func (s *RunState) TransitionToAwaitApproval(tools []responses.FunctionCallMessage) {
	s.CurrentStep = StepAwaitApproval
	s.PendingToolCalls = tools
	s.recordApprovalInterrupts()
}

// recordApprovalInterrupts ensures every currently-pending tool call has an
// Interrupt record persisted in s.Interrupts. Calls raised via
// ProcessInterrupts (workflow approval, URL elicitation) already have one;
// the rest are tool-level RequiresApproval gates and get an approval-mode
// record so the pause is persisted (ToMeta) and surfaced consistently
// rather than only synthesized on the wire.
func (s *RunState) recordApprovalInterrupts() {
	if len(s.PendingToolCalls) == 0 {
		return
	}
	if s.Interrupts == nil {
		s.Interrupts = map[string]responses.Interrupt{}
	}
	for _, tc := range s.PendingToolCalls {
		if _, ok := s.Interrupts[tc.CallID]; ok {
			continue
		}
		s.Interrupts[tc.CallID] = responses.Interrupt{
			FunctionCallMessage: tc,
			Mode:                responses.InterruptModeApproval,
		}
	}
}

// TransitionToComplete moves to the complete step and clears pending tools
func (s *RunState) TransitionToComplete() {
	s.CurrentStep = StepComplete
	s.PendingToolCalls = nil
}

// ClearPendingTools clears the pending tool calls
func (s *RunState) ClearPendingTools() {
	s.PendingToolCalls = nil
}

// HasToolsAwaitingApproval returns true if there are tools waiting for approval
func (s *RunState) HasToolsAwaitingApproval() bool {
	return len(s.ToolsAwaitingApproval) > 0
}

// PromoteAwaitingToApproval moves tools awaiting approval to pending and transitions to await state
func (s *RunState) PromoteAwaitingToApproval() {
	s.CurrentStep = StepAwaitApproval
	s.PendingToolCalls = s.ToolsAwaitingApproval
	s.ToolsAwaitingApproval = nil
	s.recordApprovalInterrupts()
}

// IsPaused returns true if the state is awaiting approval
func (s *RunState) IsPaused() bool {
	return s.CurrentStep == StepAwaitApproval
}

// IsComplete returns true if the state is complete
func (s *RunState) IsComplete() bool {
	return s.CurrentStep == StepComplete
}

// NewRunState creates initial state for a fresh run
func NewRunState() *RunState {
	return &RunState{
		CurrentStep:   StepCallLLM,
		LoopIteration: 0,
		Usage:         responses.Usage{},
	}
}

// ToMeta converts RunState to a map for storage in messages.meta
func (s *RunState) ToMeta() map[string]any {
	runStateMap := map[string]any{
		"status":                 s.getStatus(),
		"current_step":           string(s.CurrentStep),
		"loop_iteration":         s.LoopIteration,
		"usage":                  s.Usage,
		"context_tokens":         s.ContextTokens,
		"pending_context_tokens": s.PendingContextTokens,
		"traceid":                s.TraceID,
	}

	// pending_interrupts is the single canonical representation of a paused
	// run's pending calls. Each entry carries the full FunctionCallMessage
	// plus mode + payload, so PendingToolCalls and the Interrupts map are
	// reconstructed from it on load (LoadRunStateFromMeta) — we don't store
	// the same FunctionCallMessage three times. It's also the shape the
	// streaming pause chunk and the UI consume, so reload matches live.
	// (ToMeta is only persisted at await/complete, so this always covers
	// PendingToolCalls.)
	if pi := s.PendingInterrupts(); len(pi) > 0 {
		runStateMap["pending_interrupts"] = pi
	}

	if len(s.ToolsAwaitingApproval) > 0 {
		runStateMap["tools_awaiting_approval"] = s.ToolsAwaitingApproval
	}

	if len(s.QueuedApprovals) > 0 {
		runStateMap["queued_approvals"] = s.QueuedApprovals
	}

	if len(s.QueuedRejections) > 0 {
		runStateMap["queued_rejections"] = s.QueuedRejections
	}

	if len(s.QueuedMessages) > 0 {
		runStateMap["queued_messages"] = s.QueuedMessages
	}

	if len(s.PendingNestedToolCalls) > 0 {
		runStateMap["pending_nested_tool_calls"] = s.PendingNestedToolCalls
	}

	if len(s.PausedToolCalls) > 0 {
		runStateMap["paused_tool_calls"] = s.PausedToolCalls
	}

	if s.LastAgentName != "" {
		runStateMap["last_agent_name"] = s.LastAgentName
	}

	return map[string]any{
		"run_state": runStateMap,
	}
}

func (s *RunState) getStatus() RunStatus {
	switch s.CurrentStep {
	case StepCallLLM, StepExecuteTools:
		return RunStatusInProgress
	case StepAwaitApproval:
		return RunStatusPaused
	case StepComplete:
		return RunStatusCompleted
	default:
		return RunStatusError
	}
}

// metaInt reads an integer out of a meta value. ToMeta stores Go ints, so a
// persistence adapter that keeps the meta map in process returns an int, while
// one that round-trips it through JSON returns a float64 (or a json.Number
// under a decoder configured for it). Accept all of them — asserting only
// float64 silently dropped the field for in-process adapters.
func metaInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	case float32:
		return int(n), true
	case float64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	default:
		return 0, false
	}
}

// LoadRunStateFromMeta loads RunState from messages.meta
func LoadRunStateFromMeta(meta map[string]any) *RunState {
	if meta == nil {
		return nil
	}

	runStateData, ok := meta["run_state"].(map[string]any)
	if !ok {
		return nil
	}

	state := &RunState{
		Usage: responses.Usage{},
	}

	if currentStep, ok := runStateData["current_step"].(string); ok {
		state.CurrentStep = Step(currentStep)
	}

	if loopIteration, ok := metaInt(runStateData["loop_iteration"]); ok {
		state.LoopIteration = loopIteration
	}

	if contextTokens, ok := metaInt(runStateData["context_tokens"]); ok {
		state.ContextTokens = contextTokens
	}

	if pending, ok := metaInt(runStateData["pending_context_tokens"]); ok {
		state.PendingContextTokens = pending
	}

	if usageData, ok := runStateData["usage"]; ok {
		// Parse usage from meta using JSON marshaling for proper type
		// conversion. Marshal the value as-is rather than asserting it to
		// map[string]any: an adapter that has round-tripped the meta through
		// JSON hands back a map, but one that kept it in process hands back
		// the responses.Usage struct ToMeta stored.
		usageBytes, err := sonic.Marshal(usageData)
		if err == nil {
			sonic.Unmarshal(usageBytes, &state.Usage)
		}
	}

	// pending_interrupts is the canonical persisted pause: rebuild both the
	// PendingToolCalls work list and the Interrupts map (mode + payload)
	// from it, so the same FunctionCallMessage is stored only once.
	if pendingInterrupts, ok := runStateData["pending_interrupts"]; ok {
		var interrupts []responses.Interrupt
		if b, err := sonic.Marshal(pendingInterrupts); err == nil {
			sonic.Unmarshal(b, &interrupts)
		}
		if len(interrupts) > 0 {
			state.PendingToolCalls = make([]responses.FunctionCallMessage, 0, len(interrupts))
			state.Interrupts = make(map[string]responses.Interrupt, len(interrupts))
			for _, it := range interrupts {
				state.PendingToolCalls = append(state.PendingToolCalls, it.FunctionCallMessage)
				state.Interrupts[it.FunctionCallMessage.CallID] = it
			}
		}
	}

	// Back-compat: rows written before pending_interrupts became canonical
	// stored pending_tool_calls / interrupts directly. Only read them when
	// the reconstruction above didn't run.
	if state.PendingToolCalls == nil {
		if pendingToolCalls, ok := runStateData["pending_tool_calls"]; ok {
			toolCallsBytes, err := sonic.Marshal(pendingToolCalls)
			if err == nil {
				sonic.Unmarshal(toolCallsBytes, &state.PendingToolCalls)
			}
		}
	}

	if awaitingApproval, ok := runStateData["tools_awaiting_approval"]; ok {
		// Parse tools awaiting approval using JSON marshaling
		toolCallsBytes, err := sonic.Marshal(awaitingApproval)
		if err == nil {
			sonic.Unmarshal(toolCallsBytes, &state.ToolsAwaitingApproval)
		}
	}

	if queuedApprovals, ok := runStateData["queued_approvals"]; ok {
		queuedApprovalsBytes, err := sonic.Marshal(queuedApprovals)
		if err == nil {
			sonic.Unmarshal(queuedApprovalsBytes, &state.QueuedApprovals)
		}
	}

	if queuedRejections, ok := runStateData["queued_rejections"]; ok {
		queuedRejectionsBytes, err := sonic.Marshal(queuedRejections)
		if err == nil {
			sonic.Unmarshal(queuedRejectionsBytes, &state.QueuedRejections)
		}
	}

	if queuedMessages, ok := runStateData["queued_messages"]; ok {
		queuedMessagesBytes, err := sonic.Marshal(queuedMessages)
		if err == nil {
			sonic.Unmarshal(queuedMessagesBytes, &state.QueuedMessages)
		}
	}

	if pendingNestedToolCalls, ok := runStateData["pending_nested_tool_calls"]; ok {
		pendingNestedToolCallsBytes, err := sonic.Marshal(pendingNestedToolCalls)
		if err == nil {
			sonic.Unmarshal(pendingNestedToolCallsBytes, &state.PendingNestedToolCalls)
		}
	}

	if pausedToolCalls, ok := runStateData["paused_tool_calls"]; ok {
		pausedToolCallsBytes, err := sonic.Marshal(pausedToolCalls)
		if err == nil {
			sonic.Unmarshal(pausedToolCallsBytes, &state.PausedToolCalls)
		}
	}

	// Back-compat only (see pending_interrupts reconstruction above).
	if state.Interrupts == nil {
		if interrupts, ok := runStateData["interrupts"]; ok {
			interruptsBytes, err := sonic.Marshal(interrupts)
			if err == nil {
				sonic.Unmarshal(interruptsBytes, &state.Interrupts)
			}
		}
	}

	if lastAgentName, ok := runStateData["last_agent_name"].(string); ok {
		state.LastAgentName = lastAgentName
	}

	return state
}

// CollectNestedApprovalsForParent walks PendingNestedToolCalls for
// every inner CallID whose parent is parentCallID, then partitions
// those inner CallIDs by membership in QueuedApprovals /
// QueuedRejections.
//
// An inner CallID present in neither queue means the user hasn't
// decided that one yet — it's left out of both partitions. The
// inner agent will see only what's been decided and will pause again
// on the rest if any remain undecided after its own
// ProcessIncomingMessages run.
func (s *RunState) CollectNestedApprovalsForParent(parentCallID string) (approved, rejected []string) {
	// Iterate in a stable order. Map iteration order is randomized in Go,
	// which would make the resulting approved/rejected lists (and the
	// messages built from them) non-deterministic across Temporal/Restate
	// replays.
	innerIDs := make([]string, 0, len(s.PendingNestedToolCalls))
	for innerID := range s.PendingNestedToolCalls {
		innerIDs = append(innerIDs, innerID)
	}
	sort.Strings(innerIDs)

	for _, innerID := range innerIDs {
		if s.PendingNestedToolCalls[innerID] != parentCallID {
			continue
		}
		switch {
		case contains(s.QueuedRejections, innerID):
			rejected = append(rejected, innerID)
		case contains(s.QueuedApprovals, innerID):
			approved = append(approved, innerID)
		}
	}
	return approved, rejected
}

// contains is a small helper kept package-local to avoid pulling in
// slices for a single check (and to keep this file Go-version
// agnostic).
func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
