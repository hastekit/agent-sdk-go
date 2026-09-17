package agui

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/uuid"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/attachments"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/agents/skills"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// Registry is the minimal view of an SDK client the AG-UI handler
// needs. *hastekit.SDK satisfies it.
type Registry interface {
	Agent(name string) (*agents.Agent, bool)
	AgentNames() []string
}

type options struct {
	a2aBaseURL         string
	a2aHandlerOptions  func(agentName, namespace string) []a2asrv.RequestHandlerOption
	a2aAuthorizer      agents.A2AAuthorizer
	skillStore         skills.Store
	attachmentStore    attachments.UploadStore
	attachmentMaxBytes int64
	namespaceResolver  NamespaceResolver
	senderID           string
	fullHistory        bool
	keepalive          time.Duration
}

// WithSkillStore enables namespace-scoped skill management APIs and the embedded
// UI library, shared across agents. Configure an adapter over the same store
// in each participating agent's Skills. Agent names do not scope stored content.
// Protect management routes with application authorization middleware.
func WithSkillStore(store skills.Store) Option { return func(o *options) { o.skillStore = store } }

// WithAttachmentStore enables upload/download endpoints and owned file references.
// Give the same store to the agent's middleware.NewAttachmentMiddleware, which resolves
// the references on their way to the model. Files live under the handler's
// namespace (see WithNamespaceResolver), the same one its runs read them under.
func WithAttachmentStore(store attachments.UploadStore) Option {
	return func(o *options) { o.attachmentStore = store }
}

// WithAttachmentUploadLimit bounds each HTTP upload (default 20 MiB).
func WithAttachmentUploadLimit(n int64) Option {
	return func(o *options) { o.attachmentMaxBytes = n }
}

// Option configures the AG-UI handler.
type Option func(*options)

// NamespaceResolver derives a namespace from a request, typically using identity
// placed in its context by authentication middleware. It must be safe for
// concurrent requests. Returning an empty or whitespace-only namespace selects
// "default". Returning an error rejects the request with HTTP 403.
type NamespaceResolver func(*http.Request) (string, error)

// WithNamespaceResolver resolves the namespace once per API request, after route
// matching (so PathValue is available). A nil resolver uses "default". Errors
// never fall back to the default namespace. Authentication and authorization
// remain the application's responsibility.
func WithNamespaceResolver(resolve NamespaceResolver) Option {
	return func(o *options) { o.namespaceResolver = resolve }
}

// WithSenderID sets the sender attribution for messages POSTed by
// AG-UI clients (default "user").
func WithSenderID(id string) Option {
	return func(o *options) { o.senderID = id }
}

// WithFullHistory forwards the client's complete message list into
// the run instead of extracting only the new trailing turn. Use this
// when the agent has no conversation persistence and the AG-UI
// client is the source of truth for history. With persistence
// enabled (the SDK default) this would duplicate prior turns in the
// thread on every POST.
func WithFullHistory() Option {
	return func(o *options) { o.fullHistory = true }
}

// WithKeepalive sets the SSE keep-alive comment interval (default
// 15s; below the common 30-60s idle timeout of reverse proxies).
func WithKeepalive(d time.Duration) Option {
	return func(o *options) { o.keepalive = d }
}

func buildOptions(opts []Option) options {
	o := options{
		senderID:  "user",
		keepalive: 15 * time.Second,
	}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// NewHandler exposes every agent registered on the client over the
// AG-UI protocol:
//
//	GET  /a2a/                                    → A2A agent directory
//	GET  /a2a/{agent}/.well-known/agent-card.json  → A2A discovery card
//	POST /a2a/{agent}                            → A2A 1.0 JSON-RPC (including SSE)
//	GET  /agents                                  → {"agents": ["name", ...]}
//	GET  /agents/{agent}/skills                   → visible skill catalog and default enablement
//	POST /agents/{agent}/run                      → run the agent; SSE stream of AG-UI events
//	GET  /agents/{agent}/threads                  → stored conversation threads, newest first
//	GET  /agents/{agent}/threads/{thread}/messages → thread history as AG-UI messages
//
// The run endpoint accepts the canonical AG-UI RunAgentInput body and
// streams back the canonical event wire format, so any AG-UI client
// (CopilotKit's HttpAgent, raw @ag-ui/client, the embedded UI in
// pkg/agui/web) can point at it directly:
//
//	http.ListenAndServe(":8080", agui.NewHandler(client))
//
// The stop endpoint (POST /agents/{agent}/stop) ends a run already
// streaming, identified by its thread id in the resolved namespace — a separate request, since the
// run's own connection is busy streaming by then. It goes through the
// agent's broker, so it works from any replica, not only the one holding
// the SSE connection.
//
// The threads endpoints power conversation pickers. Listing requires
// the agent's persistence adapter to implement history.ThreadLister
// (the SDK's in-memory and file adapters do); when it doesn't, the
// listing endpoint answers 501 so clients can hide the picker.
func NewHandler(registry Registry, opts ...Option) http.Handler {
	o := buildOptions(opts)
	mux := http.NewServeMux()
	o.mountA2A(mux, registry)
	handleFunc := func(pattern string, fn http.HandlerFunc) {
		mux.Handle(pattern, o.withNamespace(fn))
	}
	if o.skillStore != nil {
		h := o.withNamespace(skills.NewHandler(o.skillStore, func(r *http.Request) (string, error) { return requestNamespace(r), nil }))
		mux.Handle("/skills", h)
		mux.Handle("/skills/", h)
	}
	if o.attachmentStore != nil {
		// Uploads and downloads live under this handler's namespace, the same
		// one every run it starts stores and reads attachments under.
		mux.Handle("/attachments/", o.withNamespace(attachments.NewHTTPHandler(o.attachmentStore, o.attachmentMaxBytes,
			func(r *http.Request) (string, error) { return requestNamespace(r), nil })))
	}

	handleFunc("GET /agents", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// full_history tells a client whether this server needs the whole
		// conversation on every run. It normally does not — the agent loads
		// the thread itself from ThreadID — so a client that knows this can
		// post just the new turn instead of re-uploading the thread as it
		// grows. Under WithFullHistory there is no stored thread to load
		// from, and the client's list is the only history there is.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"agents":       registry.AgentNames(),
			"full_history": o.fullHistory,
			"attachments":  o.attachmentStore != nil,
			"skill_store":  o.skillStore != nil,
		})
	})

	handleFunc("GET /agents/{agent}/skills", func(w http.ResponseWriter, r *http.Request) {
		agent, ok := registry.Agent(r.PathValue("agent"))
		if !ok {
			writeJSONError(w, http.StatusNotFound, "agent not found")
			return
		}
		catalog, err := agent.ListSkills(r.Context(), requestNamespace(r), map[string]any{"Header": collectHeaders(r.Header)}, agents.SkillSelection{})
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "unable to list skills: "+err.Error())
			return
		}
		if catalog == nil {
			catalog = []agents.ListedSkill{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"skills": catalog})
	})

	handleFunc("POST /agents/{agent}/run", func(w http.ResponseWriter, r *http.Request) {
		agent, ok := registry.Agent(r.PathValue("agent"))
		if !ok {
			writeJSONError(w, http.StatusNotFound, "agent not found")
			return
		}
		serveRun(w, r, agent, o)
	})

	handleFunc("POST /agents/{agent}/stop", func(w http.ResponseWriter, r *http.Request) {
		agent, ok := registry.Agent(r.PathValue("agent"))
		if !ok {
			writeJSONError(w, http.StatusNotFound, "agent not found")
			return
		}
		serveStop(w, r, agent)
	})

	handleFunc("GET /agents/{agent}/threads/{thread}/stream", func(w http.ResponseWriter, r *http.Request) {
		agent, ok := registry.Agent(r.PathValue("agent"))
		if !ok {
			writeJSONError(w, http.StatusNotFound, "agent not found")
			return
		}
		serveStream(w, r, agent, r.PathValue("thread"), o)
	})

	handleFunc("GET /agents/{agent}/runs", func(w http.ResponseWriter, r *http.Request) {
		agent, ok := registry.Agent(r.PathValue("agent"))
		if !ok {
			writeJSONError(w, http.StatusNotFound, "agent not found")
			return
		}
		serveRunFeed(w, r, agent, o)
	})

	handleFunc("GET /agents/{agent}/threads", func(w http.ResponseWriter, r *http.Request) {
		agent, ok := registry.Agent(r.PathValue("agent"))
		if !ok {
			writeJSONError(w, http.StatusNotFound, "agent not found")
			return
		}
		serveThreads(w, r, agent, o)
	})

	handleFunc("GET /agents/{agent}/threads/{thread}/messages", func(w http.ResponseWriter, r *http.Request) {
		agent, ok := registry.Agent(r.PathValue("agent"))
		if !ok {
			writeJSONError(w, http.StatusNotFound, "agent not found")
			return
		}
		serveThreadMessages(w, r, agent, r.PathValue("thread"), o)
	})

	return mux
}

// serveThreads lists the agent's stored threads in the handler's
// namespace, newest first. Answers 501 when the agent's persistence
// adapter can't enumerate threads.
func serveThreads(w http.ResponseWriter, r *http.Request, agent *agents.Agent, o options) {
	lister := threadLister(agent)
	if lister == nil {
		writeJSONError(w, http.StatusNotImplemented, "the agent's persistence adapter does not support thread listing")
		return
	}
	threads, err := lister.ListThreads(r.Context(), requestNamespace(r))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "unable to list threads: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"threads": threads})
}

// serveThreadMessages returns a thread's stored history converted to
// the AG-UI message shape, ready for a client to hydrate a chat from.
func serveThreadMessages(w http.ResponseWriter, r *http.Request, agent *agents.Agent, threadID string, o options) {
	manager := agent.History()
	if manager == nil || manager.ConversationPersistenceAdapter == nil {
		writeJSONError(w, http.StatusNotImplemented, "the agent has no conversation persistence")
		return
	}

	namespace := requestNamespace(r)
	opts, err := messagePageOptions(r, namespace, threadID)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	page, err := history.LoadTranscriptPage(r.Context(), manager.ConversationPersistenceAdapter, namespace, threadID, opts)
	if err != nil {
		if errors.Is(err, history.ErrInvalidTranscriptCursor) {
			writeJSONError(w, http.StatusBadRequest, err.Error())
		} else {
			writeJSONError(w, http.StatusInternalServerError, "unable to load messages: "+err.Error())
		}
		return
	}
	var latest []history.ConversationMessage
	if page.Latest != nil {
		latest = []history.ConversationMessage{*page.Latest}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"threadId":   threadID,
		"sessionId":  sessionIDFromRows(threadID, latest),
		"messages":   HistoryToMessages(page.Rows),
		"run":        threadRunState(latest),
		"nextCursor": nextMessageCursor(namespace, threadID, page.NextBeforeRunID),
		"hasMore":    page.NextBeforeRunID != "",
	})
}

// threadLister returns the agent's persistence adapter as a
// history.ThreadLister, or nil when listing isn't supported.
func threadLister(agent *agents.Agent) history.ThreadLister {
	manager := agent.History()
	if manager == nil {
		return nil
	}
	if lister, ok := manager.ConversationPersistenceAdapter.(history.ThreadLister); ok {
		return lister
	}
	return nil
}

// AgentHandler exposes a single agent's AG-UI run endpoint. Every
// POST, regardless of path, runs the agent — so it can be mounted
// anywhere on an existing mux:
//
//	mux.Handle("POST /my-agent/run", agui.AgentHandler(agent))
//
// It has no stop endpoint: every POST here starts a run, leaving no path
// to carry one. Mount NewHandler for that, or call agent.Stop with the
// run's stream id from a route of your own.
func AgentHandler(agent *agents.Agent, opts ...Option) http.Handler {
	o := buildOptions(opts)
	return o.withNamespace(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "POST a RunAgentInput to run the agent")
			return
		}
		serveRun(w, r, agent, o)
	}))
}

// serveStop stops a thread in the resolved namespace. threadId is required in
// the JSON body or query. An optional streamId must match the server-derived ID;
// a raw stream ID alone can never select a cancellation target.
func serveStop(w http.ResponseWriter, r *http.Request, agent *agents.Agent) {
	var body struct {
		ThreadID string `json:"threadId"`
		StreamID string `json:"streamId"`
	}
	if r.Body != nil {
		decoder := json.NewDecoder(r.Body)
		if err := decoder.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeJSONError(w, http.StatusBadRequest, "invalid stop request")
			return
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			writeJSONError(w, http.StatusBadRequest, "invalid stop request")
			return
		}
	}
	query := r.URL.Query()
	threadID := body.ThreadID
	if threadID == "" {
		threadID = query.Get("threadId")
	}
	if strings.TrimSpace(threadID) == "" {
		writeJSONError(w, http.StatusBadRequest, "threadId is required")
		return
	}
	// Reject conflicting body/query identifiers instead of silently ignoring one.
	for _, value := range query["threadId"] {
		if value != threadID {
			writeJSONError(w, http.StatusBadRequest, "conflicting threadId")
			return
		}
	}
	streamID := agents.StreamIDForThread(requestNamespace(r), threadID)
	supplied := append([]string{body.StreamID}, query["streamId"]...)
	for _, value := range supplied {
		if value != "" && value != streamID {
			writeJSONError(w, http.StatusForbidden, "streamId does not match the thread in the resolved namespace")
			return
		}
	}

	if err := agent.Stop(r.Context(), streamID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "unable to stop run: "+err.Error())
		return
	}

	// Accepted, not OK: the run winds down on its own connection, and a
	// stream id with no run behind it is recorded just the same.

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"threadId": threadID, "streamId": streamID, "stopping": true})
}

// serveStream attaches to a thread's run without starting one, and
// follows it to completion. It is how a client that navigated away — or
// one that never held the run's stream id — picks the run back up: the
// channel is derived from the thread, and the broker replays what the run
// has emitted so far before live chunks continue.
//
// Last-Event-ID (or lastEventId in the query) resumes after that event.
// Without a cursor, replay starts from the retained opening event, including
// completed runs. A thread with no live run or retained replay answers 204.
func serveStream(w http.ResponseWriter, r *http.Request, agent *agents.Agent, threadID string, o options) {
	if threadID == "" {
		writeJSONError(w, http.StatusBadRequest, "thread id is required")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	ctx := r.Context()
	streamID := agents.StreamIDForThread(requestNamespace(r), threadID)

	rawCursor := r.Header.Get("Last-Event-ID")
	if queryCursor := r.URL.Query().Get("lastEventId"); queryCursor != "" {
		if rawCursor != "" && rawCursor != queryCursor {
			writeJSONError(w, http.StatusBadRequest, "conflicting Last-Event-ID values")
			return
		}
		rawCursor = queryCursor
	}
	cursor, err := parseEventCursor(rawCursor)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if cursor != nil && cursor.Stream != streamID {
		writeJSONError(w, http.StatusBadRequest, "Last-Event-ID belongs to another stream")
		return
	}
	var replay []*responses.ResponseChunk
	if reader, ok := agent.StreamBroker().(agents.StreamReplayReader); ok {
		replay, err = reader.Replay(ctx, streamID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "unable to read stream replay")
			return
		}
	} else if cursor != nil {
		writeJSONError(w, http.StatusNotImplemented, "stream broker does not support resumable replay")
		return
	}
	if cursor != nil {
		if err := validateReplay(replay, threadID, streamID, cursor); err != nil {
			writeJSONError(w, http.StatusGone, errReplayUnavailable.Error())
			return
		}
	}
	if len(replay) == 0 {
		active, err := waitForRunChange(ctx, agent.StreamBroker(), streamID, false, watchWait(r, 0))
		if err != nil {
			if ctx.Err() == nil {
				writeJSONError(w, http.StatusInternalServerError, "unable to check run: "+err.Error())
			}
			return
		}
		if !active {
			w.Header().Set("X-Stream-Id", streamID)
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}

	chunks, err := agent.StreamBroker().Subscribe(ctx, streamID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "unable to join run: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // nginx
	w.Header().Set("X-Stream-Id", streamID)
	w.Header().Set("X-Agui-Thread-Id", threadID)
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	pumpEvents(w, r, chunks, threadID, streamID, cursor, o.keepalive, nil)
}

// runIDOf returns the run id a lifecycle chunk carries, or "" for chunks
// that belong to no run yet.
func runIDOf(chunk *responses.ResponseChunk) string {
	switch {
	case chunk.OfRunCreated != nil:
		return chunk.OfRunCreated.RunState.Id
	case chunk.OfRunInProgress != nil:
		return chunk.OfRunInProgress.RunState.Id
	case chunk.OfRunCompleted != nil:
		return chunk.OfRunCompleted.RunState.Id
	case chunk.OfRunPaused != nil:
		return chunk.OfRunPaused.RunState.Id
	}
	return ""
}

// serveRun decodes a RunAgentInput, executes the agent, and pumps the
// chunk stream through the translator onto the response as AG-UI SSE.
// RUN_STARTED is emitted first; RUN_FINISHED/RUN_ERROR is always last
// (synthesised if the stream closes without a terminal chunk).
func serveRun(w http.ResponseWriter, r *http.Request, agent *agents.Agent, o options) {
	var input RunAgentInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid AG-UI request body: "+err.Error())
		return
	}
	if err := input.Validate(); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	sessionID, err := attachmentSessionID(r.Context(), agent, requestNamespace(r), input.ThreadID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "unable to resolve conversation")
		return
	}
	if err := validateMessageAttachments(r.Context(), requestNamespace(r), sessionID, input.Messages, o.attachmentStore); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid or inaccessible message attachments")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	// runID identifies the AG-UI logical run, used in AG-UI event
	// payloads. Distinct from the broker stream id below.
	runID := input.RunID
	if runID == "" {
		runID = uuid.NewString()
	}

	sdkMessages := input.NewTurnSDKMessages()
	if o.fullHistory {
		sdkMessages = input.ToSDKMessages()
	}

	// Surface the AG-UI grounding context to the model as a content
	// block on the user message, wrapped in <context></context>, in
	// addition to exposing it to prompt templates via RunContext below.
	appendContextBlock(sdkMessages, input.Context)

	// A thread always streams on the same channel, so a client that
	// reconnects can find the run without having kept the id.
	streamID := agents.StreamIDForThread(requestNamespace(r), input.ThreadID)
	turn := messages.New(o.senderID, sdkMessages)

	// A turn arriving while the thread is already running folds into that
	// run rather than starting a second one on the same channel. 204: the
	// caller gets no stream of its own — the live run's stream, which it
	// can rejoin, is where the answer appears.
	if claimer, ok := agent.StreamBroker().(agents.RunClaimBroker); ok {
		started, err := claimer.EnqueueOrStart(r.Context(), streamID, []history.Message{turn})
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "unable to start run: "+err.Error())
			return
		}
		if !started {
			w.Header().Set("X-Stream-Id", streamID)
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}

	selection, _ := input.SkillSelection() // validated before claiming the run
	in := &agents.AgentInput{
		Skills:    selection,
		Namespace: requestNamespace(r),
		RunID:     runID,
		ThreadID:  input.ThreadID,
		SessionID: sessionID,
		StreamID:  streamID,
		Message:   turn,
		// Fold AG-UI context into the prompt RunContext. forwardedProps
		// and state land at top-level keys so prompt templates can
		// reach them via {{State.x}} / {{ForwardedProps.y}}.
		RunContext: map[string]any{
			"Context":        contextFromAGUI(input.Context),
			"ForwardedProps": input.ForwardedProps,
			"State":          input.State,
			"Header":         collectHeaders(r.Header),
		},
	}

	ctx := r.Context()
	handle, err := agent.Execute(ctx, in)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "unable to execute agent: "+err.Error())
		return
	}

	// SSE headers MUST be set before the first write — anything added
	// after the first flush is silently dropped.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // nginx
	w.Header().Set("X-Stream-Id", handle.StreamID)
	w.Header().Set("X-Agui-Run-Id", runID)
	w.Header().Set("X-Agui-Thread-Id", input.ThreadID)
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	pumpEvents(w, r, handle.Chunks, input.ThreadID, handle.StreamID, nil, o.keepalive, func() error {
		_, err := handle.Wait(ctx)
		return err
	})
}

// appendContextBlock renders the AG-UI grounding context as a
// <context></context> text block and appends it to the last user
// message's content, so the model sees it inline with the turn. It is
// a no-op when there is no context or no user message to attach it to.
func appendContextBlock(msgs []responses.InputMessageUnion, items []InputContext) {
	block := contextBlock(items)
	if block == "" {
		return
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		im := msgs[i].OfInputMessage
		if im == nil || im.Role != constants.RoleUser {
			continue
		}
		im.Content = append(im.Content, responses.InputContentUnion{
			OfInputText: &responses.InputTextContent{Text: block},
		})
		return
	}
}

// contextBlock formats the AG-UI context list as a single
// <context></context> string, one "description: value" line per item.
// Returns "" when there is nothing to render.
func contextBlock(items []InputContext) string {
	var b strings.Builder
	for _, c := range items {
		if c.Description == "" && c.Value == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		if c.Description != "" {
			b.WriteString(c.Description)
			b.WriteString(": ")
		}
		b.WriteString(c.Value)
	}
	if b.Len() == 0 {
		return ""
	}
	return "<context>\n" + b.String() + "\n</context>"
}

// contextFromAGUI turns the AG-UI context list into a description→value
// map suitable for prompt template substitution.
func contextFromAGUI(items []InputContext) map[string]any {
	out := make(map[string]any, len(items))
	for _, c := range items {
		if c.Description == "" {
			continue
		}
		out[c.Description] = c.Value
	}
	return out
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": msg})
}

func collectHeaders(headers http.Header) map[string]string {
	out := make(map[string]string, len(headers))
	for k, v := range headers {
		if strings.HasPrefix(k, "X-") || k == "Authorization" {
			out[strings.ReplaceAll(k, "-", "_")] = v[0]
		}
	}
	return out
}
