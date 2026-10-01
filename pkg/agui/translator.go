package agui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents/attachments"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
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

	// toolCallArgs follows each open call's arguments, keyed by item_id:
	// what the deltas streamed, and the whole arguments a provider states on
	// item_added or at the end. See flushToolCallArgs.
	toolCallArgs map[string]*toolCallArgs

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

	// An image may appear in both output_item.done and response.completed.
	emittedImageIDs map[string]bool
}

// toolCallArgs is what the client has been sent of one call's arguments, and
// the whole arguments as the provider last stated them.
type toolCallArgs struct {
	streamed string
	whole    string
}

// NewTranslator returns a fresh translator for one run.
func NewTranslator(threadID, runID string) *Translator {
	return &Translator{
		threadID:              threadID,
		runID:                 runID,
		openToolCallsByItemID: map[string]string{},
		toolCallNamesByID:     map[string]string{},
		toolCallArgs:          map[string]*toolCallArgs{},
		openSteps:             map[string]bool{},
		emittedImageIDs:       map[string]bool{},
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
		// The agent loop has exited and the paused RunState is saved, so from
		// AG-UI's perspective this run is over; the thread continues on the
		// next POST. How it ended decides what that POST carries (AG-UI 1.0):
		//
		//   - Pauses a person must answer (approvals, form and URL
		//     elicitations) end with an interrupt outcome, answered by the
		//     next run's resume entries.
		//   - A pause only on client tools is a successful run that leaves the
		//     calls unanswered, answered by tool messages on the next run. The
		//     outcome does not name them (pendingToolCallIds): clients before
		//     AG-UI 1.0, including every CopilotKit release to date, reject the
		//     field, and a 1.0 client takes the calls with no result instead.
		//     A mixed pause reports the interrupt; its client tool calls stay
		//     unanswered in the stream for the client to answer alongside.
		//
		interrupts := chunk.OfRunPaused.RunState.PendingInterrupts
		human := withoutClientTools(interrupts)
		outcome := &RunFinishedOutcome{Type: OutcomeSuccess}
		out := t.closeOpenItems()
		if len(human) > 0 {
			outcome = &RunFinishedOutcome{Type: OutcomeInterrupt, Interrupts: standardInterrupts(human)}
		}
		return append(out, &RunFinishedEvent{
			BaseEvent: baseNow(),
			ThreadID:  t.threadID,
			RunID:     t.runID,
			Outcome:   outcome,
			Usage:     tokenUsage(chunk.OfRunPaused.RunState.Usage),
		})

	case chunk.OfRunFailed != nil:
		return t.Error(fmt.Errorf("%s", chunk.OfRunFailed.RunState.Error), "agent_error")

	case chunk.OfRunCompleted != nil:
		return append(t.closeOpenItems(), &RunFinishedEvent{
			BaseEvent: baseNow(),
			ThreadID:  t.threadID,
			RunID:     t.runID,
			Outcome:   &RunFinishedOutcome{Type: OutcomeSuccess},
			Usage:     tokenUsage(chunk.OfRunCompleted.RunState.Usage),
		})
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
		// A finished response has no more argument deltas, so any call a
		// provider left open is complete. Ending it now lets a client run its
		// own tool before the server starts waiting for the result.
		out := t.closeToolCalls()
		for _, item := range chunk.OfResponseCompleted.Response.Output {
			if image := item.OfImageGenerationCall; image != nil {
				out = append(out, t.handleOutputItemDone(responses.ChunkOutputItemData{
					Type: "image_generation_call", Id: image.ID,
					Result: &image.Result, OutputFormat: &image.OutputFormat,
				})...)
			}
		}
		if ev := t.stepFinish("response"); ev != nil {
			out = append(out, ev)
		}
		return out

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
		if args := t.toolCallArgs[chunk.OfFunctionCallArgumentsDelta.ItemId]; args != nil {
			args.streamed += chunk.OfFunctionCallArgumentsDelta.Delta
		}
		return []Event{&ToolCallArgsEvent{
			BaseEvent:  baseNow(),
			ToolCallID: callID,
			Delta:      chunk.OfFunctionCallArgumentsDelta.Delta,
		}}
	}
	if chunk.OfFunctionCallArgumentsDone != nil {
		// Closed on item_done, which sends whatever the deltas left out.
		if args := t.toolCallArgs[chunk.OfFunctionCallArgumentsDone.ItemId]; args != nil && chunk.OfFunctionCallArgumentsDone.Arguments != "" {
			args.whole = chunk.OfFunctionCallArgumentsDone.Arguments
		}
		return nil
	}

	// ── Tool progress (mid-execution updates) ────────────────────
	// A live, best-effort side stream keyed by the tool call id. AG-UI has
	// no native tool-progress event, so we surface it as a hastekit.* CUSTOM
	// event (strict clients ignore it). It never opens or closes a message
	// item, so it can't leave a dangling item on RUN_FINISHED.
	// A background task starting or landing. Like tool progress, AG-UI has no
	// native event for either, and neither opens or closes a message item — so
	// they cannot leave a dangling item on RUN_FINISHED.
	//
	// Started carries the task's own stream: the run that started it is over
	// long before the task is, so a client that wants the progress subscribes
	// there rather than here.
	if event := chunk.OfSummarizationStarted; event != nil {
		return []Event{&CustomEvent{BaseEvent: baseNow(), Name: CustomNameSummarizationStarted, Value: map[string]any{"runId": event.RunID, "agentName": event.AgentName}}}
	}
	if event := chunk.OfSummarizationCompleted; event != nil {
		return []Event{&CustomEvent{BaseEvent: baseNow(), Name: CustomNameSummarizationCompleted, Value: map[string]any{"runId": event.RunID, "agentName": event.AgentName, "compacted": event.Compacted, "failed": event.Failed}}}
	}
	if event := chunk.OfContextUsage; event != nil {
		return []Event{&CustomEvent{BaseEvent: baseNow(), Name: CustomNameContextUsage, Value: ContextUsage{
			Tokens:    event.Tokens,
			AgentName: event.AgentName,
		}}}
	}

	if chunk.OfBackgroundTaskStarted != nil {
		bg := chunk.OfBackgroundTaskStarted
		return []Event{&CustomEvent{
			BaseEvent: baseNow(),
			Name:      CustomNameBackgroundTaskStarted,
			Value: map[string]any{
				"taskId":     bg.TaskID,
				"toolCallId": bg.CallID,
				"toolName":   bg.ToolName,
				"streamId":   bg.StreamID,
			},
		}}
	}

	// Completed carries the same identifiers as started, so a client that
	// joined late — the run taking the result in is usually not the run that
	// started the task — can still place it against a call.
	if chunk.OfBackgroundTaskCompleted != nil {
		bg := chunk.OfBackgroundTaskCompleted
		return []Event{&CustomEvent{
			BaseEvent: baseNow(),
			Name:      CustomNameBackgroundTaskCompleted,
			Value: map[string]any{
				"taskId":     bg.TaskID,
				"toolCallId": bg.CallID,
				"toolName":   bg.ToolName,
				"streamId":   bg.StreamID,
			},
		}}
	}

	// A turn the run has taken in. AG-UI has a native shape for this — a text
	// message with the author's role — so it needs no custom event and no
	// client-side handling beyond what a client already does with messages.
	//
	// Self-contained start/content/end under its own id, and deliberately not
	// touching openTextMessageID: this closes nothing and opens nothing, so an
	// assistant message that happened to be mid-flight stays mid-flight.
	if chunk.OfInputMessage != nil {
		im := chunk.OfInputMessage
		if parts := historyContentParts(im.ContentParts); parts != nil {
			return []Event{&CustomEvent{BaseEvent: baseNow(), Name: "input_message", Value: Message{ID: im.MessageID, Role: roleOrUser(im.Role), ContentParts: parts}}}
		}
		// The grounding context the handler appends to the user's turn is
		// scaffolding for the model, and is stripped on rehydration for the
		// same reason it is stripped here: the user did not write it.
		text := stripContextBlocks(im.Content)
		if text == "" {
			return nil
		}
		return []Event{
			&TextMessageStartEvent{
				BaseEvent: baseNow(),
				MessageID: im.MessageID,
				Role:      roleOrUser(im.Role),
			},
			&TextMessageContentEvent{
				BaseEvent: baseNow(),
				MessageID: im.MessageID,
				Delta:     text,
			},
			&TextMessageEndEvent{
				BaseEvent: baseNow(),
				MessageID: im.MessageID,
			},
		}
	}

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
		// Arguments on item_added are not sent yet: providers put a
		// placeholder there ("{}" before Anthropic streams the input) or
		// the whole arguments they go on to stream anyway (Gemini), and
		// either, followed by the deltas, is not one JSON document. They
		// stand in for the whole arguments until the provider states them.
		args := &toolCallArgs{}
		if item.Arguments != nil {
			args.whole = *item.Arguments
		}
		t.toolCallArgs[item.Id] = args
		return []Event{&ToolCallStartEvent{
			BaseEvent:       baseNow(),
			ToolCallID:      callID,
			ToolCallName:    name,
			ParentMessageID: t.openTextMessageID,
		}}

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
		if args := t.toolCallArgs[item.Id]; args != nil && item.Arguments != nil && *item.Arguments != "" {
			args.whole = *item.Arguments
		}
		out := t.flushToolCallArgs(item.Id, callID)
		return append(out, &ToolCallEndEvent{
			BaseEvent:  baseNow(),
			ToolCallID: callID,
		})

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
		if item.Result != nil && *item.Result != "" && !t.emittedImageIDs[item.Id] {
			markdown := imageMarkdown(*item.Result, derefString(item.OutputFormat, "png"))
			if markdown == "" {
				return out
			}
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
					Delta: markdown},
				&TextMessageEndEvent{BaseEvent: baseNow(), MessageID: item.Id},
			)
			if item.Id != "" {
				t.emittedImageIDs[item.Id] = true
			}
		}
		return out
	}
	return nil
}

// imageMarkdown renders live provider base64 as a data URL and persisted
// attachment references as an authorized application download URL.
func imageMarkdown(result, format string) string {
	if attachments.IsFileID(result) {
		ref, err := attachments.RefFromFileID(result)
		if err != nil {
			return ""
		}
		return "![generated image](" + attachments.URL(ref) + ")"
	}
	if format == "" {
		format = "png"
	}
	return "![generated image](data:image/" + format + ";base64," + result + ")"
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
		Outcome:   &RunFinishedOutcome{Type: OutcomeSuccess},
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
// closeToolCalls ends every tool call still streaming, in a stable order.
func (t *Translator) closeToolCalls() []Event {
	itemIDs := make([]string, 0, len(t.openToolCallsByItemID))
	for itemID := range t.openToolCallsByItemID {
		itemIDs = append(itemIDs, itemID)
	}
	sort.Strings(itemIDs)
	out := make([]Event, 0, len(itemIDs))
	for _, itemID := range itemIDs {
		callID := t.openToolCallsByItemID[itemID]
		out = append(out, t.flushToolCallArgs(itemID, callID)...)
		out = append(out, &ToolCallEndEvent{
			BaseEvent:  baseNow(),
			ToolCallID: callID,
		})
		delete(t.openToolCallsByItemID, itemID)
	}
	return out
}

// flushToolCallArgs sends, as a call ends, whatever of its whole arguments
// the deltas did not: all of them for a call that streamed none (a cached
// response, or a provider that states them only at the end), the rest for one
// cut short. What the client holds is then the whole arguments, once. Deltas
// that do not lead up to the whole arguments are left as streamed; appending
// to them could not make one document.
func (t *Translator) flushToolCallArgs(itemID, callID string) []Event {
	args := t.toolCallArgs[itemID]
	delete(t.toolCallArgs, itemID)
	if args == nil || len(args.whole) <= len(args.streamed) || !strings.HasPrefix(args.whole, args.streamed) {
		return nil
	}
	return []Event{&ToolCallArgsEvent{
		BaseEvent:  baseNow(),
		ToolCallID: callID,
		Delta:      args.whole[len(args.streamed):],
	}}
}

func (t *Translator) closeOpenItems() []Event {
	out := []Event{}
	if t.openTextMessageID != "" {
		out = append(out, &TextMessageEndEvent{
			BaseEvent: baseNow(),
			MessageID: t.openTextMessageID,
		})
		t.openTextMessageID = ""
	}
	out = append(out, t.closeToolCalls()...)
	out = append(out, t.closeReasoning()...)
	// Snapshot step names first so we can iterate safely while
	// stepFinish deletes from the map.
	if len(t.openSteps) > 0 {
		names := make([]string, 0, len(t.openSteps))
		for name := range t.openSteps {
			names = append(names, name)
		}
		sort.Strings(names)
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

// approvalCalls pulls the function calls from the approval-mode interrupts
// of a paused run: the ones a person approves or rejects.
func approvalCalls(interrupts []responses.Interrupt) []responses.FunctionCallMessage {
	out := make([]responses.FunctionCallMessage, 0, len(interrupts))
	for _, it := range interrupts {
		if it.Mode == responses.InterruptModeApproval {
			out = append(out, it.FunctionCallMessage)
		}
	}
	return out
}

// withoutClientTools drops client-tool pauses, which the client resolves by running the tool.
func withoutClientTools(interrupts []responses.Interrupt) []responses.Interrupt {
	var out []responses.Interrupt
	for _, it := range interrupts {
		if it.Mode != responses.InterruptModeClientTool {
			out = append(out, it)
		}
	}
	return out
}

// clientToolCallIDs names the client tool calls a pause leaves for the client, in order.
func clientToolCallIDs(interrupts []responses.Interrupt) []string {
	var ids []string
	for _, it := range interrupts {
		if it.Mode == responses.InterruptModeClientTool {
			ids = append(ids, it.FunctionCallMessage.CallID)
		}
	}
	return ids
}

// approvalSchema is the response an approval interrupt expects.
var approvalSchema = map[string]any{
	"type":       "object",
	"properties": map[string]any{"approved": map[string]any{"type": "boolean"}},
	"required":   []string{"approved"},
}

// standardInterrupts converts pauses a person answers into AG-UI 1.0 interrupts.
// An approval is bound to its tool call; an elicitation asks for input, with a
// form's requested schema as the response schema and a URL in metadata. The
// metadata carries the tool call and our mode so a client can render the
// prompt without the transcript.
func standardInterrupts(interrupts []responses.Interrupt) []Interrupt {
	out := make([]Interrupt, 0, len(interrupts))
	for _, it := range interrupts {
		mode := it.Mode
		if mode == "" {
			mode = responses.InterruptModeApproval
		}
		call := it.FunctionCallMessage
		entry := Interrupt{
			ID:         call.CallID,
			ToolCallID: call.CallID,
			Metadata:   map[string]any{"mode": string(mode), "toolName": call.Name, "arguments": call.Arguments},
		}
		if it.IsNested {
			entry.Metadata["isNested"] = true
		}
		switch mode {
		case responses.InterruptModeApproval:
			entry.Reason = InterruptReasonToolCall
			entry.Message = fmt.Sprintf("Allow %s to run?", call.Name)
			entry.ResponseSchema = approvalSchema
		default:
			entry.Reason = InterruptReasonInputRequired
		}
		if len(it.Elicitations) > 0 {
			first := it.Elicitations[0]
			entry.Message = first.Message
			if schema := jsonObject(first.RequestedSchema); schema != nil {
				entry.ResponseSchema = schema
			}
			if first.URL != "" {
				entry.Metadata["url"] = first.URL
			}
			if len(it.Elicitations) > 1 {
				entry.Metadata["elicitations"] = it.Elicitations
			}
		}
		out = append(out, entry)
	}
	return out
}

// jsonObject re-reads a value as a JSON object, or returns nil if it is not one.
func jsonObject(value any) map[string]any {
	if value == nil {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var out map[string]any
	if json.Unmarshal(data, &out) != nil {
		return nil
	}
	return out
}

// tokenUsage reports a run's usage in the AG-UI 1.0 shape, or nothing when unknown.
func tokenUsage(u responses.Usage) []TokenUsage {
	if u.InputTokens == 0 && u.OutputTokens == 0 && u.TotalTokens == 0 {
		return nil
	}
	return []TokenUsage{{
		InputTokens:       u.InputTokens,
		OutputTokens:      u.OutputTokens,
		TotalTokens:       u.TotalTokens,
		ReasoningTokens:   u.OutputTokensDetails.ReasoningTokens,
		CachedInputTokens: u.InputTokensDetails.CachedTokens,
	}}
}
