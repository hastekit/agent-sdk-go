package agui

import (
	"encoding/json"
	"time"

	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
)

// Translator turns a stream of responses.ResponseChunk into a stream
// of AG-UI events. It's stateful because AG-UI's event grammar
// requires explicit START/END bracketing around text messages and
// tool calls — our upstream chunks emit those bracket events too
// (response.output_item.added / .done) but with different IDs and
// indexing conventions, so we maintain the open-item book here.
//
// The translator is single-threaded: one Translator per run, one
// Translate call per chunk, on whichever goroutine the broker pumps
// from. No mutexes needed.
//
// Invariants the translator preserves so AG-UI clients don't
// desync:
//
//  1. Exactly one TEXT_MESSAGE_START / *_END pair per text message
//     item. Deltas in between always carry the same messageId.
//  2. Exactly one TOOL_CALL_START / *_END pair per function-call
//     item. ARGS deltas between them carry the same toolCallId.
//  3. RUN_STARTED always precedes any other event; RUN_FINISHED or
//     RUN_ERROR is always the last event.
//  4. If a text message is open when a tool call starts, the text
//     message is closed first (the agent loop respects this too, but
//     we double-check at item boundaries).
//  5. Every STEP_STARTED has a matching STEP_FINISHED before
//     RUN_FINISHED — @ag-ui/client's verifyEvents middleware
//     rejects the run otherwise with "Cannot send 'RUN_FINISHED'
//     while steps are still active". closeOpenItems flushes any
//     unmatched steps as a safety net.
type Translator struct {
	threadID string
	runID    string

	// Open assistant text message — empty when none is open.
	openTextMessageID string

	// Open tool calls keyed by call_id. The agent loop assigns one
	// function_call output item per tool invocation; we map its
	// item_id → call_id so subsequent argument-delta chunks (which
	// only carry item_id) can resolve back to the AG-UI toolCallId.
	openToolCallsByItemID map[string]string
	toolCallNamesByID     map[string]string

	// Open reasoning block — empty when none is open. We bracket
	// reasoning text deltas with REASONING_MESSAGE_* and the whole
	// reasoning item with REASONING_START/_END.
	openReasoningItemID string

	// Open STEP_* names. Keyed by name (not nested ids) because the
	// spec's step events have no id field — clients pair by name.
	// On an unmatched start (e.g. response.created without a
	// matching response.completed because the agent errored), this
	// guarantees closeOpenItems still emits the STEP_FINISHED so
	// the @ag-ui/client verifier doesn't reject the run.
	openSteps map[string]bool

	// Tracks whether we've emitted RUN_STARTED so duplicate
	// run.created chunks (shouldn't happen but defensive) don't
	// re-emit.
	runStarted bool
}

// NewTranslator returns a fresh translator for one run.
func NewTranslator(threadID, runID string) *Translator {
	return &Translator{
		threadID:              threadID,
		runID:                 runID,
		openToolCallsByItemID: map[string]string{},
		toolCallNamesByID:     map[string]string{},
		openSteps:             map[string]bool{},
	}
}

// stepStart bookkeeps an opened step and returns the matching event.
// Returns nil when the named step is already open (defensive — the
// agent loop shouldn't double-fire response.created, but two
// STEP_STARTED events with the same name would crash the verifier).
func (t *Translator) stepStart(name string) Event {
	if t.openSteps[name] {
		return nil
	}
	t.openSteps[name] = true
	return &StepStartedEvent{BaseEvent: baseNow(), StepName: name}
}

// stepFinish bookkeeps a closed step. Returns nil when the named
// step isn't actually open — a stray *Completed chunk without a
// preceding *InProgress (shouldn't happen, but the verifier rejects
// unmatched ends too).
func (t *Translator) stepFinish(name string) Event {
	if !t.openSteps[name] {
		return nil
	}
	delete(t.openSteps, name)
	return &StepFinishedEvent{BaseEvent: baseNow(), StepName: name}
}

// openReasoning emits the START pair for a reasoning item and records it
// as the open item. Every AG-UI reasoning event requires a messageId —
// we use the upstream item id — and REASONING_MESSAGE_START additionally
// requires role "reasoning".
func (t *Translator) openReasoning(id string) []Event {
	t.openReasoningItemID = id
	return []Event{
		&ReasoningStartEvent{BaseEvent: baseNow(), MessageID: id},
		&ReasoningMessageStartEvent{BaseEvent: baseNow(), MessageID: id, Role: "reasoning"},
	}
}

// closeReasoning emits the END pair for the currently-open reasoning item
// and clears it. Returns nil when nothing is open, so callers can append
// it unconditionally — including for empty reasoning items that open and
// close without ever streaming a content delta.
func (t *Translator) closeReasoning() []Event {
	if t.openReasoningItemID == "" {
		return nil
	}
	id := t.openReasoningItemID
	t.openReasoningItemID = ""
	return []Event{
		&ReasoningMessageEndEvent{BaseEvent: baseNow(), MessageID: id},
		&ReasoningEndEvent{BaseEvent: baseNow(), MessageID: id},
	}
}

// Start returns the events that must precede any chunk-derived
// output. Callers emit these before pumping so AG-UI clients see a
// RUN_STARTED before anything else.
func (t *Translator) Start() []Event {
	t.runStarted = true
	return []Event{
		&RunStartedEvent{
			BaseEvent: baseNow(),
			ThreadID:  t.threadID,
			RunID:     t.runID,
		},
	}
}

// Translate maps one ResponseChunk to zero or more AG-UI events.
// Order of the switch matches the union's declaration order in
// responses.ResponseChunk so it's easy to keep them in sync when new
// chunk variants land.
func (t *Translator) Translate(chunk *responses.ResponseChunk) []Event {
	if chunk == nil {
		return nil
	}

	// ── Run lifecycle ────────────────────────────────────────────
	switch {
	case chunk.OfRunCreated != nil:
		// Upstream RunCreated. We emit RUN_STARTED at handler entry
		// (before pumping) so we don't re-emit here.
		return nil

	case chunk.OfRunInProgress != nil:
		// No AG-UI analog. RUN_STARTED already signals "agent is
		// running"; a generic STEP_STARTED here would leak an
		// unmatched step (no run.in_progress.done counterpart) and
		// trip the verifier's "steps still active" check on
		// RUN_FINISHED. Skip.
		return nil

	case chunk.OfRunPaused != nil:
		// Run paused for human-in-the-loop approval. The agent loop
		// has already exited at this point — the resume path is a fresh
		// AG-UI POST carrying forwardedProps.command.resume, which loads
		// the paused RunState from history and transitions on the new
		// FunctionCallInterruptResolutionMessage. So from AG-UI's
		// perspective this run is over (RUN_FINISHED), but the thread
		// isn't (the client renders an approval UI and POSTs the
		// decisions back to continue).
		//
		// Emission order matters for AG-UI clients that drive UI
		// off events in arrival order:
		//   1. Close any open text/tool/reasoning items so the
		//      message log invariant holds.
		//   2. STATE_SNAPSHOT first — useCoAgent state-driven UIs
		//      see the awaitingApproval flag and pending calls
		//      before the custom approval event arrives.
		//   3. CUSTOM on_interrupt with the pending call list + a
		//      response-shape hint, so a frontend adapter doesn't
		//      need to read code to wire the resume.
		//   4. RUN_FINISHED last with result.status=paused so
		//      clients that track run state see the transition.
		interrupts := chunk.OfRunPaused.RunState.PendingInterrupts
		pending := projectPendingToolCalls(approvalCalls(interrupts))
		projected := projectInterrupts(interrupts)
		out := t.closeOpenItems()
		out = append(out,
			&StateSnapshotEvent{
				BaseEvent: baseNow(),
				Snapshot: map[string]any{
					"status": "paused",
					// awaitingApproval reports whether an approve/reject
					// decision is outstanding, not merely that the run is
					// paused: a run paused only on an elicitation needs data
					// or a visited URL, not a verdict. A client that gates an
					// approval prompt on this flag would otherwise render an
					// empty prompt for a form pause.
					"awaitingApproval": len(pending) > 0,
					"pendingToolCalls": pending,
					// interrupts carries every pending pause with its mode and
					// payload. pendingToolCalls stays approval-only so clients
					// written against it keep working.
					"interrupts": projected,
					"threadId":   t.threadID,
					"runId":      t.runID,
				},
			},
			&CustomEvent{
				BaseEvent: baseNow(),
				// "on_interrupt" is CopilotKit's useInterrupt event
				// name and the de-facto AG-UI convention; LangGraph
				// follows it too. The hook only fires after the
				// matching RUN_FINISHED (onRunFinalized), so the
				// emission order below (event → RUN_FINISHED) is
				// load-bearing.
				Name: CustomNameInterrupt,
				Value: map[string]any{
					// "kind" disambiguates our interrupt subtype for
					// frontends that handle multiple agent types under
					// the same useInterrupt hook. It stays "tool_approval"
					// whenever every pause is an approval, so existing
					// clients are unaffected by elicitation support.
					"kind":             interruptKind(interrupts),
					"runId":            t.runID,
					"threadId":         t.threadID,
					"pendingToolCalls": pending,
					"interrupts":       projected,
					// Self-describing resume contract.
					// CopilotKit's useInterrupt resolves to whatever
					// shape the application chooses; we expect an
					// array of decisions matching interrupts.
					"resume": map[string]any{
						"method":     "POST",
						"forwardKey": "command.resume",
						"shape": map[string]any{
							"decisions": []map[string]string{{
								"toolCallId": "string (matches interrupts[].toolCallId)",
								"approved":   "boolean",
								"content": "object — the answer for a data-carrying interrupt: " +
									"form fields matching interrupts[].requestedSchema. Omit for " +
									"plain approvals and for url mode.",
							}},
						},
					},
				},
			},
			&RunFinishedEvent{
				BaseEvent: baseNow(),
				ThreadID:  t.threadID,
				RunID:     t.runID,
				Result: map[string]any{
					"status": "paused",
					"usage":  chunk.OfRunPaused.RunState.Usage,
				},
			},
		)
		return out

	case chunk.OfRunCompleted != nil:
		out := t.closeOpenItems()
		out = append(out,
			&RunFinishedEvent{
				BaseEvent: baseNow(),
				ThreadID:  t.threadID,
				RunID:     t.runID,
				Result: map[string]any{
					"status": "completed",
					"usage":  chunk.OfRunCompleted.RunState.Usage,
				},
			},
		)
		return out
	}

	// ── Response lifecycle ───────────────────────────────────────
	switch {
	case chunk.OfResponseCreated != nil:
		// Each LLM call inside the agent loop emits a response.created.
		// Treat as a sub-step so the UI sees the per-turn boundary.
		if ev := t.stepStart("response"); ev != nil {
			return []Event{ev}
		}
		return nil

	case chunk.OfResponseCompleted != nil:
		if ev := t.stepFinish("response"); ev != nil {
			return []Event{ev}
		}
		return nil

	case chunk.OfResponseInProgress != nil:
		return nil
	}

	// ── Output item lifecycle ────────────────────────────────────
	if chunk.OfOutputItemAdded != nil {
		return t.handleOutputItemAdded(chunk.OfOutputItemAdded.Item)
	}
	if chunk.OfOutputItemDone != nil {
		return t.handleOutputItemDone(chunk.OfOutputItemDone.Item)
	}

	// ── Text deltas ──────────────────────────────────────────────
	if chunk.OfOutputTextDelta != nil {
		// item_id from the upstream chunk is the assistant message id
		// AG-UI clients track. If we somehow get a delta before the
		// matching item_added (shouldn't happen) we lazily open the
		// message so the stream stays valid.
		mid := chunk.OfOutputTextDelta.ItemId
		out := []Event{}
		if t.openTextMessageID != mid {
			if t.openTextMessageID != "" {
				out = append(out, &TextMessageEndEvent{
					BaseEvent: baseNow(),
					MessageID: t.openTextMessageID,
				})
			}
			out = append(out, &TextMessageStartEvent{
				BaseEvent: baseNow(),
				MessageID: mid,
				Role:      RoleAssistant,
			})
			t.openTextMessageID = mid
		}
		out = append(out, &TextMessageContentEvent{
			BaseEvent: baseNow(),
			MessageID: mid,
			Delta:     chunk.OfOutputTextDelta.Delta,
		})
		return out
	}

	if chunk.OfOutputTextDone != nil {
		// item_done will close the message; the explicit text.done
		// chunk is just a marker that the upstream is finished
		// streaming this text part. No-op (we close on item_done).
		return nil
	}

	if chunk.OfOutputTextAnnotationAdded != nil {
		// Annotations (citations) — AG-UI has no first-class field,
		// surface as CUSTOM so a frontend that wants citations can
		// render them.
		return []Event{&CustomEvent{
			BaseEvent: baseNow(),
			Name:      CustomNameAnnotation,
			Value: map[string]any{
				"messageId":  chunk.OfOutputTextAnnotationAdded.ItemId,
				"annotation": chunk.OfOutputTextAnnotationAdded.Annotation,
				"index":      chunk.OfOutputTextAnnotationAdded.AnnotationIndex,
			},
		}}
	}

	// ── Function call argument deltas ────────────────────────────
	if chunk.OfFunctionCallArgumentsDelta != nil {
		callID, ok := t.openToolCallsByItemID[chunk.OfFunctionCallArgumentsDelta.ItemId]
		if !ok {
			// Defensive — the agent loop should always emit item_added first.
			return nil
		}
		return []Event{&ToolCallArgsEvent{
			BaseEvent:  baseNow(),
			ToolCallID: callID,
			Delta:      chunk.OfFunctionCallArgumentsDelta.Delta,
		}}
	}
	if chunk.OfFunctionCallArgumentsDone != nil {
		// Closed on item_done.
		return nil
	}

	// ── Tool progress (mid-execution updates) ────────────────────
	// A live, best-effort side stream keyed by the tool call id. AG-UI has
	// no native tool-progress event, so we surface it as a hastekit.* CUSTOM
	// event (strict clients ignore it). It never opens or closes a message
	// item, so it can't leave a dangling item on RUN_FINISHED.
	if chunk.OfToolProgress != nil {
		tp := chunk.OfToolProgress
		return []Event{&CustomEvent{
			BaseEvent: baseNow(),
			Name:      CustomNameToolProgress,
			Value: map[string]any{
				"toolCallId": tp.CallID,
				"toolName":   tp.ToolName,
				"progress":   tp.Progress,
				"total":      tp.Total,
				"message":    tp.Message,
				"sequence":   tp.SequenceNumber,
			},
		}}
	}

	// ── Function call output (the tool's result) ─────────────────
	if chunk.OfFunctionCallOutput != nil {
		fco := chunk.OfFunctionCallOutput
		content := ""
		if fco.Output.OfString != nil {
			content = *fco.Output.OfString
		} else if fco.Output.OfList != nil {
			// Serialise the structured output list so the UI receives
			// a string payload (CopilotKit expects content: string).
			if b, err := json.Marshal(fco.Output.OfList); err == nil {
				content = string(b)
			}
		}
		return []Event{&ToolCallResultEvent{
			BaseEvent:  baseNow(),
			MessageID:  fco.ID,
			ToolCallID: fco.CallID,
			Content:    content,
			Role:       RoleTool,
		}}
	}

	// Reasoning event helpers live on the translator (openReasoning /
	// closeReasoning, defined below) so every emission site supplies the
	// messageId the AG-UI schema requires.

	// ── Reasoning text (OSS-only) ────────────────────────────────
	if chunk.OfReasoningTextDelta != nil {
		out := []Event{}
		if t.openReasoningItemID != chunk.OfReasoningTextDelta.ItemId {
			out = append(out, t.closeReasoning()...)
			out = append(out, t.openReasoning(chunk.OfReasoningTextDelta.ItemId)...)
		}
		out = append(out, &ReasoningMessageContentEvent{
			BaseEvent: baseNow(),
			MessageID: t.openReasoningItemID,
			Delta:     chunk.OfReasoningTextDelta.Delta,
		})
		return out
	}

	// ── Reasoning summary (provider-hosted reasoning models) ─────
	if chunk.OfReasoningSummaryTextDelta != nil {
		out := []Event{}
		if t.openReasoningItemID != chunk.OfReasoningSummaryTextDelta.ItemId {
			out = append(out, t.closeReasoning()...)
			out = append(out, t.openReasoning(chunk.OfReasoningSummaryTextDelta.ItemId)...)
		}
		out = append(out, &ReasoningMessageContentEvent{
			BaseEvent: baseNow(),
			MessageID: t.openReasoningItemID,
			Delta:     chunk.OfReasoningSummaryTextDelta.Delta,
		})
		return out
	}

	// ── Image generation: partial frames are dropped. The model
	// streams several partial_image chunks plus a final result for one
	// image; emitting each as its own event produced duplicate images
	// in the UI. We surface only the completed image, as a markdown
	// image message in handleOutputItemDone — one image, one message,
	// and it renders identically live and on history reload.
	if chunk.OfImageGenerationCallPartialImage != nil {
		return nil
	}

	// Web search & code interpreter: emit STEP boundaries so the UI
	// can render "Searching the web…" / "Running code…" hints
	// without us inventing a custom shape. Routed through
	// stepStart/stepFinish so closeOpenItems can flush any stragglers
	// when the run terminates early.
	switch {
	case chunk.OfWebSearchCallInProgress != nil:
		if ev := t.stepStart("web_search"); ev != nil {
			return []Event{ev}
		}
		return nil
	case chunk.OfWebSearchCallCompleted != nil:
		if ev := t.stepFinish("web_search"); ev != nil {
			return []Event{ev}
		}
		return nil
	case chunk.OfCodeInterpreterCallInProgress != nil:
		if ev := t.stepStart("code_interpreter"); ev != nil {
			return []Event{ev}
		}
		return nil
	case chunk.OfCodeInterpreterCallCompleted != nil:
		if ev := t.stepFinish("code_interpreter"); ev != nil {
			return []Event{ev}
		}
		return nil
	}

	// Anything we don't explicitly map: drop. RAW forwarding is
	// noisy and we'd rather curate the surface. Add a case above
	// when a new chunk variant needs to reach the UI.
	return nil
}

// handleOutputItemAdded opens the AG-UI bracket events that match
// the upstream item's type.
func (t *Translator) handleOutputItemAdded(item responses.ChunkOutputItemData) []Event {
	switch item.Type {
	case "message":
		// Close any other open text message first (shouldn't happen
		// in practice but keeps the invariant true).
		out := []Event{}
		if t.openTextMessageID != "" && t.openTextMessageID != item.Id {
			out = append(out, &TextMessageEndEvent{
				BaseEvent: baseNow(),
				MessageID: t.openTextMessageID,
			})
		}
		t.openTextMessageID = item.Id
		role := RoleAssistant
		if item.Role != "" {
			role = Role(item.Role)
		}
		out = append(out, &TextMessageStartEvent{
			BaseEvent: baseNow(),
			MessageID: item.Id,
			Role:      role,
		})
		return out

	case "function_call":
		if item.CallID == nil {
			return nil
		}
		callID := *item.CallID
		name := ""
		if item.Name != nil {
			name = *item.Name
		}
		t.openToolCallsByItemID[item.Id] = callID
		t.toolCallNamesByID[callID] = name
		out := []Event{&ToolCallStartEvent{
			BaseEvent:       baseNow(),
			ToolCallID:      callID,
			ToolCallName:    name,
			ParentMessageID: t.openTextMessageID,
		}}
		// Some chunks ship the full arguments on item_added when the
		// model didn't stream them (cached responses). Emit them as a
		// single ARGS event so CopilotKit's incremental renderer
		// still receives delta data.
		if item.Arguments != nil && *item.Arguments != "" {
			out = append(out, &ToolCallArgsEvent{
				BaseEvent:  baseNow(),
				ToolCallID: callID,
				Delta:      *item.Arguments,
			})
		}
		return out

	case "reasoning":
		out := []Event{}
		if t.openReasoningItemID != item.Id {
			out = append(out, t.closeReasoning()...)
		}
		out = append(out, t.openReasoning(item.Id)...)
		return out

	case "image_generation_call":
		// In-progress signal so UI shows "Generating image…".
		if ev := t.stepStart("image_generation"); ev != nil {
			return []Event{ev}
		}
		return nil
	}
	return nil
}

// handleOutputItemDone closes the AG-UI bracket events that match
// the upstream item's type, and (for terminal items like image gen)
// fires off the actual payload event.
func (t *Translator) handleOutputItemDone(item responses.ChunkOutputItemData) []Event {
	switch item.Type {
	case "message":
		if t.openTextMessageID != item.Id {
			return nil
		}
		t.openTextMessageID = ""
		return []Event{&TextMessageEndEvent{
			BaseEvent: baseNow(),
			MessageID: item.Id,
		}}

	case "function_call":
		callID, ok := t.openToolCallsByItemID[item.Id]
		if !ok && item.CallID != nil {
			callID = *item.CallID
			ok = true
		}
		if !ok {
			return nil
		}
		delete(t.openToolCallsByItemID, item.Id)
		return []Event{&ToolCallEndEvent{
			BaseEvent:  baseNow(),
			ToolCallID: callID,
		}}

	case "reasoning":
		if t.openReasoningItemID != item.Id {
			return nil
		}
		return t.closeReasoning()

	case "image_generation_call":
		out := []Event{}
		if ev := t.stepFinish("image_generation"); ev != nil {
			out = append(out, ev)
		}
		if item.Result != nil && *item.Result != "" {
			// Close any open assistant text message first so the image
			// message's START doesn't nest inside it.
			if t.openTextMessageID != "" {
				out = append(out, &TextMessageEndEvent{BaseEvent: baseNow(), MessageID: t.openTextMessageID})
				t.openTextMessageID = ""
			}
			// Surface the completed image as its own assistant text
			// message carrying a markdown image (data URL). This renders
			// in any markdown-capable client and, because it's a real
			// message, survives history reload (see HistoryToMessages).
			out = append(out,
				&TextMessageStartEvent{BaseEvent: baseNow(), MessageID: item.Id, Role: RoleAssistant},
				&TextMessageContentEvent{BaseEvent: baseNow(), MessageID: item.Id,
					Delta: imageMarkdown(*item.Result, derefString(item.OutputFormat, "png"))},
				&TextMessageEndEvent{BaseEvent: baseNow(), MessageID: item.Id},
			)
		}
		return out
	}
	return nil
}

// imageMarkdown renders a base64 image as a markdown image with a data
// URL, the representation generated images take in the AG-UI message
// stream and in reloaded history.
func imageMarkdown(base64, format string) string {
	if format == "" {
		format = "png"
	}
	return "![generated image](data:image/" + format + ";base64," + base64 + ")"
}

// Error closes the run with an error event. Returns the events the
// caller should write before tearing down the SSE connection.
func (t *Translator) Error(err error, code string) []Event {
	if err == nil {
		return nil
	}
	out := t.closeOpenItems()
	out = append(out, &RunErrorEvent{
		BaseEvent: baseNow(),
		Message:   err.Error(),
		Code:      code,
	})
	return out
}

// Finish closes the run without a terminal chunk having arrived —
// the safety net for a broker stream that closed cleanly but never
// delivered run.completed. closeOpenItems keeps the message log
// valid; the synthetic RUN_FINISHED keeps strict clients from
// hanging on an open run.
func (t *Translator) Finish() []Event {
	out := t.closeOpenItems()
	out = append(out, &RunFinishedEvent{
		BaseEvent: baseNow(),
		ThreadID:  t.threadID,
		RunID:     t.runID,
		Result:    map[string]any{"status": "completed"},
	})
	return out
}

// closeOpenItems emits the END events for any items still open. Used
// at run termination and on paused-run boundaries to keep the AG-UI
// message log in a valid state — and, critically, to satisfy
// @ag-ui/client's verifier which rejects RUN_FINISHED while any
// STEP_* or *_MESSAGE_*/TOOL_CALL_* are still open.
//
// Order matters: messages → tools → reasoning → steps. Steps wrap
// finer-grained items in the spec, so they close last.
func (t *Translator) closeOpenItems() []Event {
	out := []Event{}
	if t.openTextMessageID != "" {
		out = append(out, &TextMessageEndEvent{
			BaseEvent: baseNow(),
			MessageID: t.openTextMessageID,
		})
		t.openTextMessageID = ""
	}
	for itemID, callID := range t.openToolCallsByItemID {
		out = append(out, &ToolCallEndEvent{
			BaseEvent:  baseNow(),
			ToolCallID: callID,
		})
		delete(t.openToolCallsByItemID, itemID)
	}
	out = append(out, t.closeReasoning()...)
	// Snapshot step names first so we can iterate safely while
	// stepFinish deletes from the map.
	if len(t.openSteps) > 0 {
		names := make([]string, 0, len(t.openSteps))
		for name := range t.openSteps {
			names = append(names, name)
		}
		for _, name := range names {
			if ev := t.stepFinish(name); ev != nil {
				out = append(out, ev)
			}
		}
	}
	return out
}

// baseNow stamps the current millisecond Unix timestamp onto a fresh
// BaseEvent. Centralised so we don't drift between events.
func baseNow() BaseEvent {
	return BaseEvent{Timestamp: time.Now().UnixMilli()}
}

func derefString(p *string, fallback string) string {
	if p == nil {
		return fallback
	}
	return *p
}

// projectPendingToolCalls converts the SDK's FunctionCallMessage
// shape (snake_case JSON tags) into a camelCase projection so the
// AG-UI client receives a consistent casing across STATE_SNAPSHOT
// and CUSTOM events. We could let json.Marshal do its native thing
// on FunctionCallMessage, but that leaks the SDK's wire choice into
// AG-UI consumers and the namespace mismatch (camelCase wrapper +
// snake_case payload) is exactly the kind of detail that breaks
// CopilotKit's TypeScript types.
func projectPendingToolCalls(calls []responses.FunctionCallMessage) []map[string]any {
	out := make([]map[string]any, 0, len(calls))
	for _, c := range calls {
		out = append(out, map[string]any{
			"toolCallId":   c.CallID,
			"toolCallName": c.Name,
			"arguments":    c.Arguments,
		})
	}
	return out
}

// approvalCalls pulls the function calls from the approval-mode interrupts
// of a paused run — the subset the pendingToolCalls field carries, which is
// approve/reject decisions only. Every mode, including these, also appears in
// the richer projectInterrupts list.
func approvalCalls(interrupts []responses.Interrupt) []responses.FunctionCallMessage {
	out := make([]responses.FunctionCallMessage, 0, len(interrupts))
	for _, it := range interrupts {
		if it.Mode == responses.InterruptModeApproval {
			out = append(out, it.FunctionCallMessage)
		}
	}
	return out
}

// projectInterrupts converts every pending interrupt into the camelCase shape
// AG-UI consumers receive, carrying the mode and its payload so a client can
// render the right thing: an approve/reject prompt, a form built from the
// requested JSON schema, or a link to visit.
//
// An interrupt may carry several elicitations. The first supplies the
// top-level message/schema/url — the common case is exactly one, and a client
// that handles more reads the full elicitations list.
func projectInterrupts(interrupts []responses.Interrupt) []map[string]any {
	out := make([]map[string]any, 0, len(interrupts))
	for _, it := range interrupts {
		mode := it.Mode
		if mode == "" {
			mode = responses.InterruptModeApproval
		}
		entry := map[string]any{
			"toolCallId":   it.FunctionCallMessage.CallID,
			"toolCallName": it.FunctionCallMessage.Name,
			"arguments":    it.FunctionCallMessage.Arguments,
			"mode":         string(mode),
		}
		if it.IsNested {
			entry["isNested"] = true
		}
		if len(it.Elicitations) > 0 {
			first := it.Elicitations[0]
			if first.Message != "" {
				entry["message"] = first.Message
			}
			if first.RequestedSchema != nil {
				entry["requestedSchema"] = first.RequestedSchema
			}
			if first.URL != "" {
				entry["url"] = first.URL
			}
			if len(it.Elicitations) > 1 {
				entry["elicitations"] = it.Elicitations
			}
		}
		out = append(out, entry)
	}
	return out
}

// interruptKind labels the pause for frontends that switch on it. It stays
// "tool_approval" when every interrupt is an approval so clients written
// before elicitation support keep matching on the value they know.
func interruptKind(interrupts []responses.Interrupt) string {
	var approvals, elicitations int
	for _, it := range interrupts {
		if it.Mode == "" || it.Mode == responses.InterruptModeApproval {
			approvals++
			continue
		}
		elicitations++
	}
	switch {
	case elicitations == 0:
		return "tool_approval"
	case approvals == 0:
		return "elicitation"
	default:
		return "mixed"
	}
}
