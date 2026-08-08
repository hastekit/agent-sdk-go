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
	MessageID      string         `json:"message_id" db:"message_id"`
	ThreadID       string         `json:"thread_id" db:"thread_id"`
	ConversationID string         `json:"conversation_id" db:"conversation_id"`
	Messages       []Message      `json:"messages" db:"messages"`
	Meta           map[string]any `json:"meta" db:"meta"`
}

// Message re-exported from the messages package so callers can keep referring to history.Message
type Message = messages.Message

// Summary represents a conversation summary stored in the summaries table
type Summary struct {
	ID                      string         `json:"id" db:"id"`
	ThreadID                string         `json:"thread_id" db:"thread_id"`
	SummaryMessage          Message        `json:"summary_message" db:"summary_message"`
	LastSummarizedMessageID string         `json:"last_summarized_message_id" db:"last_summarized_message_id"`
	CreatedAt               time.Time      `json:"created_at" db:"created_at"`
	Meta                    map[string]any `json:"meta" db:"meta"`
}

type ConversationPersistenceAdapter interface {
	NewConversationID(ctx context.Context) string
	NewRunID(ctx context.Context) string
	LoadMessages(ctx context.Context, namespace string, threadID string, previousMessageID string) ([]ConversationMessage, error)
	SaveMessages(ctx context.Context, namespace, msgId, previousMsgId, threadID string, conversationId string, messages []Message, meta map[string]any) error
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

	Options []ConversationManagerOptions
}

func NewConversationManager(p ConversationPersistenceAdapter, opts ...ConversationManagerOptions) *CommonConversationManager {
	cm := &CommonConversationManager{
		ConversationPersistenceAdapter: p,
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

type ConversationRunManager struct {
	ConversationPersistenceAdapter

	namespace      string
	conversationId string
	msgId          string
	previousMsgId  string
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
}

func NewRun(ctx context.Context, cm *CommonConversationManager, namespace string, threadID string, previousMessageID string, options ...RunOption) (*ConversationRunManager, error) {
	cr := &ConversationRunManager{
		ConversationPersistenceAdapter: cm.ConversationPersistenceAdapter,
		summarizer:                     cm.Summarizer,
		messageFilter:                  cm.MessageFilter,
		messageAttribution:             cm.MessageAttribution,
		msgIdToRunId:                   make(map[string]string),
		State:                          make(map[string]string),
	}

	// Load messages
	err := cr.LoadMessages(ctx, namespace, threadID, previousMessageID)
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
		var contextTokens int
		if cr.RunState != nil {
			lastAgent = cr.RunState.LastAgentName
			contextTokens = cr.RunState.ContextTokens
		}
		cr.RunState = agentstate.NewRunState()
		cr.RunState.LastAgentName = lastAgent
		cr.RunState.ContextTokens = contextTokens
	} else {
		// Continuing the previous run
		runID = cr.previousMsgId
	}

	// Store the run id
	cr.msgId = runID

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

func (cm *ConversationRunManager) AddMessages(ctx context.Context, message Message) {
	cm.ProcessIncomingMessages(message, false)
}

func (cm *ConversationRunManager) AddMessagesToQueue(ctx context.Context, msgs []Message) {
	for _, m := range msgs {
		cm.ProcessIncomingMessages(m, true)
	}
}

func (cm *ConversationRunManager) GetMessages(ctx context.Context, agentName string) ([]responses.InputMessageUnion, error) {
	cm.RunState.LastAgentName = agentName

	// Process messages with summarizer if available
	if cm.summarizer != nil {
		summaryResult, err := cm.summarizer.Summarize(ctx, cm.msgIdToRunId, cm.oldMessages, cm.RunState.ContextTokens)
		if err != nil {
			return nil, err
		}

		// If a summary was created, track it for saving later and apply it to messages
		if summaryResult != nil {
			cm.summaries = summaryResult
			if summaryResult.Summary == nil {
				cm.oldMessages = summaryResult.MessagesToKeep
			} else {
				cm.oldMessages = append([]Message{*summaryResult.Summary}, summaryResult.MessagesToKeep...)
			}
		}
	}

	// Add queued messages to the new messages
	if len(cm.RunState.QueuedMessages) > 0 {
		cm.newMessages = append(cm.newMessages, cm.RunState.QueuedMessages...)
		cm.RunState.QueuedMessages = nil
	}

	msgList := append(cm.oldMessages, cm.newMessages...)
	if cm.messageFilter != nil {
		msgList = cm.messageFilter.Filter(ctx, msgList, agentName)
	}
	return cm.attributeMessages(msgList, agentName), nil
}

func (cm *ConversationRunManager) LoadMessages(ctx context.Context, namespace string, threadID string, previousMessageID string) error {
	cm.threadId = threadID

	if cm.ConversationPersistenceAdapter == nil {
		return nil
	}

	// Don't have to reload
	if len(cm.oldMessages) > 0 {
		return nil
	}

	convMessages, err := cm.ConversationPersistenceAdapter.LoadMessages(ctx, namespace, threadID, previousMessageID)
	if err != nil {
		return err
	}

	oldMessages := []Message{}
	for _, msg := range convMessages {
		for _, bundle := range msg.Messages {
			cm.msgIdToRunId[bundle.ID] = msg.MessageID
		}
		cm.threadId = msg.ThreadID
		cm.conversationId = msg.ConversationID
		cm.previousMsgId = msg.MessageID

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
func (cm *ConversationRunManager) GetMessageID() string {
	return cm.msgId
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
			ID:                      cm.summaries.SummaryID,
			ThreadID:                cm.threadId,
			LastSummarizedMessageID: cm.summaries.LastSummarizedMessageID,
			CreatedAt:               time.Now(),
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
		err := cm.ConversationPersistenceAdapter.SaveMessages(ctx, cm.namespace, cm.msgId, cm.previousMsgId, cm.threadId, cm.conversationId, cm.newMessages, meta)
		if err != nil {
			return err
		}
	}

	runState := agentstate.LoadRunStateFromMeta(meta)
	if runState.IsComplete() {
		cm.previousMsgId = cm.msgId
		cm.msgId = uuid.NewString()
	}

	cm.lastMessageMeta = meta
	cm.oldMessages = append(cm.oldMessages, cm.newMessages...)
	cm.newMessages = nil

	return nil
}

func (cm *ConversationRunManager) TrackUsage(usage *responses.Usage) {
	if usage == nil {
		return
	}
	cm.RunState.Usage.InputTokens += usage.InputTokens
	cm.RunState.Usage.OutputTokens += usage.OutputTokens
	cm.RunState.Usage.InputTokensDetails.CachedTokens += usage.InputTokensDetails.CachedTokens
	cm.RunState.Usage.TotalTokens += usage.TotalTokens

	// ContextTokens tracks the most recent call's size
	cm.RunState.ContextTokens = usage.TotalTokens
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

func (cm *ConversationRunManager) ProcessIncomingMessages(message Message, queue bool) {
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
