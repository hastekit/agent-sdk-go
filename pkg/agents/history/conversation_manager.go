package history

import (
	"context"
	"log/slog"
	"time"

	"github.com/bytedance/sonic"
	"github.com/google/uuid"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/messages"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
	"go.opentelemetry.io/otel"
)

var (
	tracer = otel.Tracer("History")
)

// ConversationMessage represents a turn within a thread.
type ConversationMessage struct {
	RunID          string         `json:"run_id" db:"run_id"`
	ThreadID       string         `json:"thread_id" db:"thread_id"`
	ConversationID string         `json:"conversation_id" db:"conversation_id"`
	Messages       []Message      `json:"messages" db:"messages"`
	Meta           map[string]any `json:"meta" db:"meta"`
}

// Message re-exported from the messages package so callers can keep referring to history.Message
type Message = messages.Message

// Summary represents a conversation summary stored in the summaries table
type Summary struct {
	ID                  string         `json:"id" db:"id"`
	ThreadID            string         `json:"thread_id" db:"thread_id"`
	SummaryMessage      Message        `json:"summary_message" db:"summary_message"`
	LastSummarizedRunID string         `json:"last_summarized_run_id" db:"last_summarized_run_id"`
	CreatedAt           time.Time      `json:"created_at" db:"created_at"`
	Meta                map[string]any `json:"meta" db:"meta"`
}

type ConversationPersistenceAdapter interface {
	NewConversationID(ctx context.Context) string
	NewRunID(ctx context.Context) string
	LoadMessages(ctx context.Context, namespace string, threadID string, previousRunID string) ([]ConversationMessage, error)
	SaveMessages(ctx context.Context, namespace, runId, previousRunId, threadID string, conversationId string, messages []Message, meta map[string]any) error
	SaveSummary(ctx context.Context, namespace string, summary Summary) error
}

type CommonConversationManager struct {
	ConversationPersistenceAdapter ConversationPersistenceAdapter
	Summarizer                     HistorySummarizer
	MessageFilter                  MessageFilter

	// MessageAttribution controls whether sender-attributed bundles are
	// rewritten with "(Agent)/(Human) <sender> said:" prefixes before being
	// sent to the provider. It is opt-in — off by default, in which case
	// bundles are flattened into the provider message list as-is.
	MessageAttribution bool

	// SteeringNotices controls whether a message that arrived mid-run is
	// followed by a note saying so. On by default; see WithoutSteeringNotices
	// for when to turn it off.
	SteeringNotices bool

	Options []ConversationManagerOptions
}

func NewConversationManager(p ConversationPersistenceAdapter, opts ...ConversationManagerOptions) *CommonConversationManager {
	cm := &CommonConversationManager{
		ConversationPersistenceAdapter: p,
		SteeringNotices:                true,
	}

	for _, o := range opts {
		o(cm)
	}

	return cm
}

type ConversationManagerOptions func(*CommonConversationManager)

func WithSummarizer(summarizer HistorySummarizer) ConversationManagerOptions {
	return func(cm *CommonConversationManager) {
		cm.Summarizer = summarizer
	}
}

func WithMessageFilter(filter MessageFilter) ConversationManagerOptions {
	return func(cm *CommonConversationManager) {
		cm.MessageFilter = filter
	}
}

// WithMessageAttribution enables multi-participant message attribution, which
// rewrites messages from other senders with "(Agent)/(Human) <sender> said:"
// prefixes before sending them to the provider. Attribution is off by default.
func WithMessageAttribution() ConversationManagerOptions {
	return func(cm *CommonConversationManager) {
		cm.MessageAttribution = true
	}
}

// WithoutSteeringNotices suppresses the note that normally follows a message
// which arrived mid-run, leaving such messages indistinguishable from one that
// opened the turn.
//
// Turn it off only when something upstream already conveys the distinction, or
// when an agent's prompt depends on the exact message sequence. The note costs
// a few dozen tokens per interjection and is otherwise worth keeping: without
// it the model cannot tell a correction shouted mid-task from the instruction
// it is already carrying out.
func WithoutSteeringNotices() ConversationManagerOptions {
	return func(cm *CommonConversationManager) {
		cm.SteeringNotices = false
	}
}

type ConversationRunManager struct {
	ConversationPersistenceAdapter

	namespace      string
	conversationId string
	runId          string
	previousRunId  string
	msgIdToRunId   map[string]string
	threadId       string

	convMessages    []ConversationMessage
	oldMessages     []Message
	newMessages     []Message
	lastMessageMeta map[string]any

	// runContext
	runContext map[string]any

	// State is used to store any key-value pairs that need to be persisted along with the run
	State map[string]string

	// RunState is used to store the state of the run, such as the current step and the usage of the run
	RunState *agentstate.RunState

	summarizer         HistorySummarizer
	summaries          *SummaryResult
	messageFilter      MessageFilter
	messageAttribution bool

	steeringNotices bool
	// steeredIDs holds the bundles this run drained off the queue — the ones
	// the user sent while the run was already working. It is deliberately not
	// persisted: the note it drives is about *when* a message arrived relative
	// to the run that received it, which stops being useful once that run ends
	// and the message is simply part of the thread's history.
	steeredIDs map[string]struct{}
}

func NewRun(ctx context.Context, cm *CommonConversationManager, namespace string, threadID string, previousRunID string, options ...RunOption) (*ConversationRunManager, error) {
	cr := &ConversationRunManager{
		ConversationPersistenceAdapter: cm.ConversationPersistenceAdapter,
		summarizer:                     cm.Summarizer,
		messageFilter:                  cm.MessageFilter,
		messageAttribution:             cm.MessageAttribution,
		steeringNotices:                cm.SteeringNotices,
		msgIdToRunId:                   make(map[string]string),
		State:                          make(map[string]string),
	}

	// Load messages
	err := cr.LoadMessages(ctx, namespace, threadID, previousRunID)
	if err != nil {
		return nil, err
	}

	// Load the run state
	var runID string
	if cr.RunState == nil || cr.RunState.IsComplete() {
		// Create a new run id
		runID = cr.ConversationPersistenceAdapter.NewRunID(ctx)

		// Carry the last responding agent forward into the fresh run so a
		// new turn can resume where the previous one left off (sticky
		// handoff). Nothing else reads LastAgentName, so this is inert
		// unless an agent opts into sticky routing.
		//
		// ContextTokens carries forward too: it measures how full the
		// context window already is, which is a property of the thread, not
		// of a single run. Resetting it to zero would make the first
		// GetMessages of every turn see "no context yet" and skip
		// summarization — so an agent that answers in one LLM call per turn
		// (no tool loop) would never summarize at all.
		var lastAgent string
		var contextTokens, pendingContextTokens int
		if cr.RunState != nil {
			lastAgent = cr.RunState.LastAgentName
			contextTokens = cr.RunState.ContextTokens
			// The estimate carries too, though it is usually zero here: a run
			// that ends normally does so straight after an LLM call, and that
			// call's usage report cleared it. The exception is a cancelled run,
			// which appends synthetic tool results and a cancellation notice
			// and then completes without calling the model again — leaving real
			// messages in history that nothing has measured. Carrying a zero
			// costs nothing; dropping a non-zero one would under-count the next
			// turn.
			pendingContextTokens = cr.RunState.PendingContextTokens
		}
		cr.RunState = agentstate.NewRunState()
		cr.RunState.LastAgentName = lastAgent
		cr.RunState.ContextTokens = contextTokens
		cr.RunState.PendingContextTokens = pendingContextTokens
	} else {
		// Continuing the previous run
		runID = cr.previousRunId
	}

	// Store the run id
	cr.runId = runID

	// Run the options
	for _, o := range options {
		o(cr)
	}

	if cr.conversationId == "" {
		cr.conversationId = cr.ConversationPersistenceAdapter.NewConversationID(ctx)
	}

	return cr, nil
}

type RunOption func(manager *ConversationRunManager)

func WithConversationID(cid string) RunOption {
	return func(cm *ConversationRunManager) {
		cm.conversationId = cid
	}
}

// RunContextMetaKey is the saved row meta entry under which
// SaveMessages records the run's RunContext (see WithRunContext).
const RunContextMetaKey = "run_context"

// WithRunContext records the run's RunContext, which SaveMessages
// stores in the saved row's meta under RunContextMetaKey. It is
// persistence-only — never sent to the provider — so later reads can
// recover the context a turn ran under (e.g. the inbound sender).
func WithRunContext(rc map[string]any) RunOption {
	return func(cm *ConversationRunManager) {
		cm.runContext = rc
	}
}

// AddMessageOption adjusts how an appended message is accounted for.
type AddMessageOption func(*addMessageConfig)

type addMessageConfig struct {
	estimate bool
}

// AlreadyMeasured marks a bundle whose tokens the most recent usage report
// already covers, so it is appended without being estimated on top.
//
// Exactly one thing qualifies: the model's own reply, appended straight after
// TrackUsage, which counted it as part of that call's reported total. Anything
// else added during a run — tool results, user turns, synthetic messages — has
// not been weighed by any provider yet and must be estimated, so it takes the
// default.
func AlreadyMeasured() AddMessageOption {
	return func(c *addMessageConfig) { c.estimate = false }
}

func (cm *ConversationRunManager) AddMessages(ctx context.Context, message Message, opts ...AddMessageOption) {
	cfg := addMessageConfig{estimate: true}
	for _, o := range opts {
		o(&cfg)
	}
	cm.processIncoming(message, false, cfg.estimate)
}

func (cm *ConversationRunManager) AddMessagesToQueue(ctx context.Context, msgs []Message) {
	for _, m := range msgs {
		cm.processIncoming(m, true, true)
	}
}

func (cm *ConversationRunManager) GetMessages(ctx context.Context, agentName string) ([]responses.InputMessageUnion, error) {
	cm.RunState.LastAgentName = agentName

	if cm.summarizer != nil {
		if err := cm.summarize(ctx); err != nil {
			return nil, err
		}
	}

	// Queued messages join this run's messages after summarization. Draining
	// earlier would let the summarizer see them, but it could not act on them:
	// they belong to the run in flight, which every summarizer keeps whole. They
	// already carry this run's id from when they were queued — QueuedMessages is
	// not carried into a fresh RunState, so a queued message is only ever
	// drained by the same run that queued it.
	if len(cm.RunState.QueuedMessages) > 0 {
		// Everything on the queue arrived after this run started working —
		// that is what the queue is for. Record the bundles so the outgoing
		// list can say so; they are otherwise indistinguishable from the turn
		// that opened the run.
		for _, m := range cm.RunState.QueuedMessages {
			cm.markSteered(m)
		}
		cm.newMessages = append(cm.newMessages, cm.RunState.QueuedMessages...)
		cm.RunState.QueuedMessages = nil
	}

	// Build the outgoing list into its own backing array. `append(cm.oldMessages,
	// ...)` writes the run's messages into oldMessages' spare capacity whenever
	// it has any, so the filter below would mutate the history it was handed.
	msgList := make([]Message, 0, len(cm.oldMessages)+len(cm.newMessages))
	msgList = append(msgList, cm.oldMessages...)
	msgList = append(msgList, cm.newMessages...)

	if cm.messageFilter != nil {
		msgList = cm.messageFilter.Filter(ctx, msgList, agentName)
	}
	return cm.attributeMessages(msgList, agentName), nil
}

// summarize hands the summarizer the whole outgoing conversation — the history
// loaded from persistence *and* everything this run has produced so far — and
// applies what it decides to trim.
//
// Passing both halves changes the decision, not just the bookkeeping. A run's
// own output is the fastest-growing part of the request: each loop iteration
// appends an assistant turn plus its tool results, and none of it reaches
// persistence until the run reaches a terminal step. A summarizer shown only
// the loaded history sees a conversation that has stopped growing, and declines
// to act while the request it is being asked about keeps expanding.
//
// Only the loaded half is reshaped by the result. cm.newMessages is also the
// save buffer — SaveMessages persists exactly those messages — so dropping from
// it here would leave a hole in the thread's history. Both shipped summarizers
// keep the most recent run whole and every in-flight message belongs to it, so
// the partition below defends an invariant rather than changing behaviour.
func (cm *ConversationRunManager) summarize(ctx context.Context) error {
	candidates := make([]Message, 0, len(cm.oldMessages)+len(cm.newMessages))
	candidates = append(candidates, cm.oldMessages...)
	candidates = append(candidates, cm.newMessages...)

	result, err := cm.summarizer.Summarize(ctx, cm.msgIdToRunId, candidates, cm.contextTokens())
	if err != nil {
		return err
	}
	if result == nil {
		return nil
	}

	// Bill what the summary cost, without touching ContextTokens — the size of a
	// summarization request says nothing about the agent's context window.
	cm.TrackAuxiliaryUsage(result.Usage)

	// A summary is persisted as "history up to and including run X is covered",
	// and a boundary naming the run still being written is not a claim this
	// manager should make. No summarizer here produces one — both floor their
	// keep window at one run, so the boundary always stops short of the most
	// recent — and the bundled persistence adapter would ignore it anyway: it
	// applies a summary only when the named run is strictly older than the row
	// being loaded, which by then is a run that finished saving. Neither of
	// those is guaranteed by an interface a caller can implement, so refuse the
	// claim rather than rely on both holding. Clearing it leaves the summary row
	// saved but inert on reload.
	if result.LastSummarizedRunID == cm.runId {
		slog.WarnContext(ctx, "summarizer covered the in-flight run; not persisting the summary boundary",
			"run_id", cm.runId)
		result.LastSummarizedRunID = ""
	}

	cm.summaries = result

	inFlight := make(map[string]struct{}, len(cm.newMessages))
	for _, m := range cm.newMessages {
		if m.ID != "" {
			inFlight[m.ID] = struct{}{}
		}
	}

	kept := make([]Message, 0, len(result.MessagesToKeep))
	for _, m := range result.MessagesToKeep {
		if _, ok := inFlight[m.ID]; ok {
			// Already held in the save buffer; GetMessages appends it after
			// oldMessages, so keeping it here too would send it twice.
			continue
		}
		kept = append(kept, m)
	}

	if result.Summary != nil {
		// Register the summary under its own run id, the same way LoadMessages
		// does when it reads the summary row back. Without this the summary is
		// unplaced for the rest of this run and groups with whatever unplaced
		// message sits next to it, so the run the summarizer sees in memory
		// differs from the one it sees after a reload.
		if result.Summary.ID != "" {
			cm.msgIdToRunId[result.Summary.ID] = result.SummaryID
		}
		cm.oldMessages = append([]Message{*result.Summary}, kept...)
	} else {
		cm.oldMessages = kept
	}

	return nil
}

// markSteered records that a bundle reached this run mid-flight rather than
// opening it.
func (cm *ConversationRunManager) markSteered(m Message) {
	if m.ID == "" || !cm.steeringNotices {
		return
	}
	if cm.steeredIDs == nil {
		cm.steeredIDs = map[string]struct{}{}
	}
	cm.steeredIDs[m.ID] = struct{}{}
}

// trackRun records which run a bundle belongs to. LoadMessages does this for
// persisted history; without the same for messages produced during the run,
// every in-flight bundle looks up to the empty run id and the summarizer groups
// them into one nameless pseudo-run — pooled with the re-injected summary, whose
// id is also empty.
func (cm *ConversationRunManager) trackRun(m Message) {
	if m.ID == "" {
		return
	}
	if cm.msgIdToRunId == nil {
		cm.msgIdToRunId = map[string]string{}
	}
	cm.msgIdToRunId[m.ID] = cm.runId
}

func (cm *ConversationRunManager) LoadMessages(ctx context.Context, namespace string, threadID string, previousRunID string) error {
	cm.threadId = threadID

	if cm.ConversationPersistenceAdapter == nil {
		return nil
	}

	// Don't have to reload
	if len(cm.oldMessages) > 0 {
		return nil
	}

	convMessages, err := cm.ConversationPersistenceAdapter.LoadMessages(ctx, namespace, threadID, previousRunID)
	if err != nil {
		return err
	}

	oldMessages := []Message{}
	for _, msg := range convMessages {
		for _, bundle := range msg.Messages {
			cm.msgIdToRunId[bundle.ID] = msg.RunID
		}
		cm.threadId = msg.ThreadID
		cm.conversationId = msg.ConversationID
		cm.previousRunId = msg.RunID

		oldMessages = append(oldMessages, msg.Messages...)

		// Store the most recent message's meta for run state loading
		// The last message in the chain contains the current run state
		if msg.Meta != nil {
			cm.lastMessageMeta = msg.Meta
		}
	}

	// Initialize lastMessageMeta if no messages were found
	if cm.lastMessageMeta == nil {
		cm.lastMessageMeta = make(map[string]any)
	}

	cm.namespace = namespace
	cm.convMessages = convMessages
	cm.oldMessages = oldMessages
	cm.RunState = agentstate.LoadRunStateFromMeta(cm.lastMessageMeta)
	cm.loadSubAgentContext(ctx)

	return nil
}

// GetMeta returns the meta from the most recent message
func (cm *ConversationRunManager) GetMeta() map[string]any {
	return cm.lastMessageMeta
}

// GetMessageID returns the current run id
func (cm *ConversationRunManager) GetRunID() string {
	return cm.runId
}

// GetConversationID GetOrCreateConversationID returns the conversation ID, if it doesn't exist it will create one
func (cm *ConversationRunManager) GetConversationID() string {
	return cm.conversationId
}

func (cm *ConversationRunManager) SaveMessages(ctx context.Context) error {
	meta := cm.RunState.ToMeta()
	if meta == nil {
		meta = map[string]any{}
	}

	meta["state"] = cm.State

	// Record the run's RunContext (persistence-only; never sent to the
	// provider) so later reads recover the turn's context.
	if len(cm.runContext) > 0 {
		meta[RunContextMetaKey] = cm.runContext
	}

	if cm.summaries != nil {
		sum := Summary{
			ID:                  cm.summaries.SummaryID,
			ThreadID:            cm.threadId,
			LastSummarizedRunID: cm.summaries.LastSummarizedRunID,
			CreatedAt:           time.Now(),
			Meta: map[string]any{
				"is_summary": true,
			},
		}

		if cm.summaries.Summary != nil {
			sum.SummaryMessage = *cm.summaries.Summary
		}

		if cm.ConversationPersistenceAdapter != nil {
			err := cm.ConversationPersistenceAdapter.SaveSummary(ctx, cm.namespace, sum)
			if err != nil {
				return err
			}
		}

		cm.summaries = nil
	}

	if cm.ConversationPersistenceAdapter != nil {
		err := cm.ConversationPersistenceAdapter.SaveMessages(ctx, cm.namespace, cm.runId, cm.previousRunId, cm.threadId, cm.conversationId, cm.newMessages, meta)
		if err != nil {
			return err
		}
	}

	runState := agentstate.LoadRunStateFromMeta(meta)
	if runState.IsComplete() {
		cm.previousRunId = cm.runId
		cm.runId = uuid.NewString()
	}

	cm.lastMessageMeta = meta
	cm.oldMessages = append(cm.oldMessages, cm.newMessages...)
	cm.newMessages = nil

	return nil
}

// TrackUsage records a call made *as* the agent — one whose input was the
// conversation itself. It bills the tokens and refreshes the measured half of
// the context-occupancy signal.
//
// TotalTokens, because the signal predicts the size of the *next* prompt, and
// both halves of the total are in it: the prompt just sent, plus the reply,
// which is appended to history a moment later. Both are numbers the provider
// gave us, so neither belongs in the estimate — and the reply least of all,
// since a reasoning-heavy one is mostly opaque thinking blocks the estimator
// cannot see inside and would score near zero.
func (cm *ConversationRunManager) TrackUsage(usage *responses.Usage) {
	if usage == nil {
		return
	}
	cm.accumulateUsage(usage)

	// Everything up to and including the reply is now measured, so the running
	// estimate that stood in for it has served its purpose. The reply is
	// appended with AlreadyMeasured so it is not counted a second time.
	cm.RunState.ContextTokens = usage.TotalTokens
	cm.RunState.PendingContextTokens = 0
}

// contextTokens is what the summarizer triggers on: the last measured prompt
// plus an estimate of everything appended since. Neither half is sufficient
// alone — the measurement is always at least one call stale, and the estimate
// only covers the delta.
func (cm *ConversationRunManager) contextTokens() int {
	return cm.RunState.ContextTokens + cm.RunState.PendingContextTokens
}

// ContextTokens reports how full the context window is by that same reckoning,
// for callers outside this package — a pre-call budget check, say, which wants
// the estimate the next prompt will be built from.
func (cm *ConversationRunManager) ContextTokens() int {
	return cm.contextTokens()
}

// TrackAuxiliaryUsage bills a call made *on behalf of* the run against some
// other prompt — summarization being the one that exists today. Those tokens
// are real spend and belong in the run's total, but their size says nothing
// about how full the agent's context window is: a summarization request is its
// own instruction plus a flattened transcript, with none of the agent's tools or
// system prompt. Folding it into ContextTokens would hand the summarizer a
// reading of a conversation that is not the one it is deciding about.
func (cm *ConversationRunManager) TrackAuxiliaryUsage(usage *responses.Usage) {
	if usage == nil {
		return
	}
	cm.accumulateUsage(usage)
}

func (cm *ConversationRunManager) accumulateUsage(usage *responses.Usage) {
	cm.RunState.Usage.InputTokens += usage.InputTokens
	cm.RunState.Usage.OutputTokens += usage.OutputTokens
	cm.RunState.Usage.InputTokensDetails.CachedTokens += usage.InputTokensDetails.CachedTokens
	cm.RunState.Usage.OutputTokensDetails.ReasoningTokens += usage.OutputTokensDetails.ReasoningTokens
	cm.RunState.Usage.TotalTokens += usage.TotalTokens
}

func (cm *ConversationRunManager) loadSubAgentContext(ctx context.Context) {
	data := cm.lastMessageMeta["state"]

	if data == nil {
		return
	}

	buf, err := sonic.Marshal(data)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal state", "error", err)
		return
	}

	if err = sonic.Unmarshal(buf, &cm.State); err != nil {
		slog.ErrorContext(ctx, "failed to unmarshal state", "error", err)
		return
	}
}

// ProcessIncomingMessages appends an inbound message, estimating its size
// against the context window. Use AddMessages with AlreadyMeasured to append
// one the provider has already counted.
func (cm *ConversationRunManager) ProcessIncomingMessages(message Message, queue bool) {
	cm.processIncoming(message, queue, true)
}

func (cm *ConversationRunManager) processIncoming(message Message, queue, estimate bool) {
	// Process incoming message, and extract tool approvals and user messages
	hasNewApproval := false
	var stored []responses.InputMessageUnion
	for _, msg := range message.Messages {
		if msg.OfFunctionCallInterruptResolution != nil {
			// Interrupt resume path. approve/reject actions drain onto the
			// QueuedApprovals/QueuedRejections queues. A data-carrying
			// resolution (Content set, e.g. a submitted form) is also kept
			// on RunState.Resolutions so the resuming tool receives the full
			// resolution — Content included — through its ResumeMessages.
			r := msg.OfFunctionCallInterruptResolution
			for _, res := range r.Resolutions {
				switch res.Action {
				case responses.InterruptActionApprove:
					hasNewApproval = true
					cm.RunState.QueuedApprovals = append(cm.RunState.QueuedApprovals, res.CallID)
					if len(res.Content) > 0 {
						if cm.RunState.Resolutions == nil {
							cm.RunState.Resolutions = map[string]responses.InterruptResolution{}
						}
						cm.RunState.Resolutions[res.CallID] = res
					}
				case responses.InterruptActionReject:
					hasNewApproval = true
					cm.RunState.QueuedRejections = append(cm.RunState.QueuedRejections, res.CallID)
				}
			}
		} else {
			stored = append(stored, msg)
		}
	}

	if len(stored) > 0 {
		// Persist only the real messages. Interrupt resolutions were drained
		// into the queues above and must never reach history or the LLM
		// provider — their type isn't a valid provider input item, so a
		// replayed bundle that still carried one would be rejected. When the
		// bundle came mixed (resolution prepended to a turn's messages, as
		// the AG-UI handler does), drop the resolution; otherwise keep the
		// original bundle as-is.
		bundle := message
		if len(stored) != len(message.Messages) {
			bundle.Messages = stored
		}
		cm.trackRun(bundle)

		// Unless a usage report already covers this message, it reaches the
		// provider unweighed on the next call. Estimate it so the summarizer is
		// not deciding against a reading that predates it. Queued messages count
		// from the moment they are queued: draining only moves them between
		// slices.
		if estimate {
			cm.RunState.PendingContextTokens += estimateBundleTokens(bundle)
		}

		if queue {
			cm.RunState.QueuedMessages = append(cm.RunState.QueuedMessages, bundle)
		} else {
			cm.newMessages = append(cm.newMessages, bundle)
		}
	}

	// If we are waiting for approval, and got an new approval message, move to execute tools
	//  - If new approval is not received, let the user messages be in the queue
	if cm.RunState.CurrentStep == agentstate.StepAwaitApproval {
		if hasNewApproval {
			cm.RunState.CurrentStep = agentstate.StepExecuteTools
		}
	}
}

// ProcessInterrupts records the bookkeeping for a paused tool call across
// every interrupt mode (human approval, URL elicitation, …). It tracks the
// nested parent↔child relationship — so agent→tool→agent→tool resume works
// for all modes — and stashes the Interrupt record (mode + payload) in
// RunState.Interrupts so the pause chunk can surface mode-specific data to
// the UI.
func (cm *ConversationRunManager) ProcessInterrupts(parentToolCall responses.FunctionCallMessage, interrupts []responses.Interrupt) {
	if cm.RunState.PendingNestedToolCalls == nil {
		cm.RunState.PendingNestedToolCalls = map[string]string{}
	}
	if cm.RunState.PausedToolCalls == nil {
		cm.RunState.PausedToolCalls = map[string]responses.FunctionCallMessage{}
	}
	if cm.RunState.Interrupts == nil {
		cm.RunState.Interrupts = map[string]responses.Interrupt{}
	}

	for _, intr := range interrupts {
		// This is the tool that has raised the interrupt
		call := intr.FunctionCallMessage

		cm.RunState.ToolsAwaitingApproval = append(cm.RunState.ToolsAwaitingApproval, call)

		// Track the parent so resume can flatten the nested call.
		cm.RunState.PendingNestedToolCalls[call.CallID] = parentToolCall.CallID

		// Nested only when the interrupt bubbled up from a different
		// (inner) call than the parent the loop is pausing on. A top-level
		// tool raising its own interrupt has parent == call.
		intr.IsNested = call.CallID != parentToolCall.CallID
		cm.RunState.Interrupts[call.CallID] = intr
	}

	// Add the parent tool call to the paused tool calls so the loop
	// re-executes it (with ShouldResume) once the user resolves.
	cm.RunState.PausedToolCalls[parentToolCall.CallID] = parentToolCall
}
