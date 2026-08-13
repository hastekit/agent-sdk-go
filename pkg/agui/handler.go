package agui

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/history"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/messages"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
)

// Registry is the minimal view of an SDK client the AG-UI handler
// needs. *hastekit.SDK satisfies it.
type Registry interface {
	Agent(name string) (*agents.Agent, bool)
	AgentNames() []string
}

type options struct {
	namespace   string
	senderID    string
	fullHistory bool
	keepalive   time.Duration
}

// Option configures the AG-UI handler.
type Option func(*options)

// WithNamespace sets the conversation namespace (default "default").
func WithNamespace(ns string) Option {
	return func(o *options) { o.namespace = ns }
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
		namespace: "default",
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
//	GET  /agents                                  → {"agents": ["name", ...]}
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
// streaming, identified by its stream id — a separate request, since the
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

	mux.HandleFunc("GET /agents", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"agents": registry.AgentNames()})
	})

	mux.HandleFunc("POST /agents/{agent}/run", func(w http.ResponseWriter, r *http.Request) {
		agent, ok := registry.Agent(r.PathValue("agent"))
		if !ok {
			writeJSONError(w, http.StatusNotFound, "agent not found")
			return
		}
		serveRun(w, r, agent, o)
	})

	mux.HandleFunc("POST /agents/{agent}/stop", func(w http.ResponseWriter, r *http.Request) {
		agent, ok := registry.Agent(r.PathValue("agent"))
		if !ok {
			writeJSONError(w, http.StatusNotFound, "agent not found")
			return
		}
		serveStop(w, r, agent)
	})

	mux.HandleFunc("GET /agents/{agent}/threads/{thread}/stream", func(w http.ResponseWriter, r *http.Request) {
		agent, ok := registry.Agent(r.PathValue("agent"))
		if !ok {
			writeJSONError(w, http.StatusNotFound, "agent not found")
			return
		}
		serveStream(w, r, agent, r.PathValue("thread"), o)
	})

	mux.HandleFunc("GET /agents/{agent}/threads", func(w http.ResponseWriter, r *http.Request) {
		agent, ok := registry.Agent(r.PathValue("agent"))
		if !ok {
			writeJSONError(w, http.StatusNotFound, "agent not found")
			return
		}
		serveThreads(w, r, agent, o)
	})

	mux.HandleFunc("GET /agents/{agent}/threads/{thread}/messages", func(w http.ResponseWriter, r *http.Request) {
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
	threads, err := lister.ListThreads(r.Context(), o.namespace)
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
	// The transcript, not the model's view of it: LoadMessages returns a
	// summary in place of the turns it covers, which is right for a prompt and
	// wrong for a chat window the user is scrolling back through.
	rows, err := manager.LoadTranscript(r.Context(), o.namespace, threadID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "unable to load messages: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"threadId": threadID,
		"messages": HistoryToMessages(rows),
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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "POST a RunAgentInput to run the agent")
			return
		}
		serveRun(w, r, agent, o)
	})
}

// serveStop stops a run, identified by the stream id the run endpoint
// returned (the X-Stream-Id header, or the streamId CUSTOM event), taken
// from a {"streamId": …} body or a ?streamId= query parameter.
//
// The stopping run ends on its own SSE connection with RUN_FINISHED,
// which is where a client should watch for the outcome.
//
// The agent in the path selects whose broker to ask, which matters when
// agents use different brokers. It is not an ownership check: the stream
// id is the capability, and only the client that started the run has it.
func serveStop(w http.ResponseWriter, r *http.Request, agent *agents.Agent) {
	var body struct {
		StreamID string `json:"streamId"`
	}
	// An empty body is fine when the stream id is in the query string.
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	streamID := body.StreamID
	if streamID == "" {
		streamID = r.URL.Query().Get("streamId")
	}
	if streamID == "" {
		writeJSONError(w, http.StatusBadRequest, "streamId is required: pass the stream id the run endpoint returned")
		return
	}

	if err := agent.Stop(r.Context(), streamID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "unable to stop run: "+err.Error())
		return
	}

	// Accepted, not OK: the run winds down on its own connection, and a
	// stream id with no run behind it is recorded just the same.

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"streamId": streamID, "stopping": true})
}

// serveStream attaches to a thread's run without starting one, and
// follows it to completion. It is how a client that navigated away — or
// one that never held the run's stream id — picks the run back up: the
// channel is derived from the thread, and the broker replays what the run
// has emitted so far before live chunks continue.
//
// A thread with no run in flight answers 204, so a client can attach
// without checking first and without being left holding a stream that
// nothing will ever publish to.
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
	streamID := agents.StreamIDForThread(o.namespace, threadID)

	// Subscribing to a channel with no run behind it would block until the
	// client gives up: nothing publishes to it and nothing closes it.
	active, err := agent.StreamBroker().IsActive(ctx, streamID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "unable to check run: "+err.Error())
		return
	}
	if !active {
		w.Header().Set("X-Stream-Id", streamID)
		w.WriteHeader(http.StatusNoContent)
		return
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

	enc := NewEncoder(w)

	// The run id comes off the replayed run.created chunk, so the events
	// this stream emits carry the same ids as the run's own connection.
	// Until then the translator has nothing to attribute events to, which
	// is why chunks are translated only once a run is seen.
	var translator *Translator

	keepalive := time.NewTicker(o.keepalive)
	defer keepalive.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			if err := enc.Comment("keepalive"); err != nil {
				return
			}
		case chunk, ok := <-chunks:
			if !ok {
				if translator != nil {
					_ = enc.EncodeAll(ctx, translator.Finish())
				}
				return
			}

			if translator == nil {
				runID := runIDOf(chunk)
				if runID == "" {
					// Nothing to attribute this to yet — the run's opening
					// chunk hasn't been replayed.
					continue
				}
				translator = NewTranslator(threadID, runID)
				if err := enc.EncodeAll(ctx, translator.Start()); err != nil {
					return
				}
			}

			if events := translator.Translate(chunk); len(events) > 0 {
				if err := enc.EncodeAll(ctx, events); err != nil {
					return
				}
			}
			if chunk.OfRunCompleted != nil || chunk.OfRunPaused != nil {
				return
			}
		}
	}
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
	streamID := agents.StreamIDForThread(o.namespace, input.ThreadID)
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

	in := &agents.AgentInput{
		Namespace: o.namespace,
		ThreadID:  input.ThreadID,
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

	enc := NewEncoder(w)
	translator := NewTranslator(input.ThreadID, runID)

	// Emit RUN_STARTED before any chunk-derived event. Also ship a
	// CUSTOM event carrying the broker StreamID so a client that
	// wants to correlate with the SDK's streaming surface can.
	if err := enc.EncodeAll(ctx, translator.Start()); err != nil {
		return
	}
	_ = enc.Encode(ctx, &CustomEvent{
		BaseEvent: baseNow(),
		Name:      CustomNameStreamID,
		Value: map[string]any{
			"streamId": handle.StreamID,
			"runId":    runID,
			"threadId": input.ThreadID,
		},
	})

	// Keep-alive ticker keeps idle SSE connections from being reaped
	// by reverse proxies.
	keepalive := time.NewTicker(o.keepalive)
	defer keepalive.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			if err := enc.Comment("keepalive"); err != nil {
				return
			}
		case chunk, ok := <-handle.Chunks:
			if !ok {
				// Stream closed without a terminal chunk. Surface the
				// run error if there is one, otherwise synthesise a
				// RUN_FINISHED so the AG-UI client doesn't hang.
				if _, err := handle.Wait(); err != nil {
					_ = enc.EncodeAll(ctx, translator.Error(err, "run_error"))
				} else {
					_ = enc.EncodeAll(ctx, translator.Finish())
				}
				return
			}
			if events := translator.Translate(chunk); len(events) > 0 {
				if err := enc.EncodeAll(ctx, events); err != nil {
					return
				}
			}
			// Run terminated — close the connection. The translator
			// has already emitted RUN_FINISHED (completed or paused).
			if chunk.OfRunCompleted != nil || chunk.OfRunPaused != nil {
				return
			}
		}
	}
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
