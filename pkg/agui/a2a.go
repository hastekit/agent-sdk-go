package agui

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
)

// WithA2ABaseURL sets the public origin used in discovery cards, for example
// https://agents.example.com. Include a reverse-proxy path prefix if needed.
// By default the request's scheme and Host are used; forwarded headers are not trusted.
func WithA2ABaseURL(baseURL string) Option {
	return func(o *options) { o.a2aBaseURL = strings.TrimRight(baseURL, "/") }
}

// WithA2AHandlerOptions configures each agent/namespace's A2A server. The factory
// is called once per adapter; supplied task stores and queues must be scoped to
// that agent and namespace. Defaults are process-local memory implementations.
func WithA2AHandlerOptions(factory func(agentName, namespace string) []a2asrv.RequestHandlerOption) Option {
	return func(o *options) { o.a2aHandlerOptions = factory }
}

// WithA2AAuthorizer supplies the host's out-of-band approval/authorization flow.
// The handler receives the resolved namespace and thread in AgentInput.
func WithA2AAuthorizer(authorizer agents.A2AAuthorizer) Option {
	return func(o *options) { o.a2aAuthorizer = authorizer }
}

// a2aKey names an adapter by agent name, not pointer: a config-built agent is
// rebuilt whenever its config changes, and keying by pointer would keep every
// version's adapter forever.
type a2aKey struct {
	agent     string
	namespace string
}

type a2aAdapter struct {
	agent   *agents.Agent
	adapter *agents.A2A
}
type a2aRoutes struct {
	registry Registry
	options  options
	mu       sync.Mutex
	adapters map[a2aKey]a2aAdapter
}

func (o options) mountA2A(mux *http.ServeMux, registry Registry) {
	routes := &a2aRoutes{registry: registry, options: o, adapters: map[a2aKey]a2aAdapter{}}
	directory := o.withNamespace(http.HandlerFunc(routes.directory))
	mux.Handle("GET /a2a", directory)
	mux.Handle("GET /a2a/{$}", directory)
	mux.Handle("GET /a2a/{agent}/.well-known/agent-card.json", o.withNamespace(http.HandlerFunc(routes.card)))
	mux.Handle("POST /a2a/{agent}", o.withNamespace(http.HandlerFunc(routes.invoke)))
	mux.Handle("POST /a2a/{agent}/{$}", o.withNamespace(http.HandlerFunc(routes.invoke)))
}

func (h *a2aRoutes) baseURL(r *http.Request) string {
	if h.options.a2aBaseURL != "" {
		return h.options.a2aBaseURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
func (h *a2aRoutes) endpoint(r *http.Request, name string) string {
	return h.baseURL(r) + "/api/agui/a2a/" + url.PathEscape(name)
}
func (h *a2aRoutes) directory(w http.ResponseWriter, r *http.Request) {
	type entry struct {
		Name         string `json:"name"`
		URL          string `json:"url"`
		AgentCardURL string `json:"agentCardUrl"`
	}
	entries := []entry{}
	names, err := h.registry.AgentNames(r.Context(), requestNamespace(r))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "unable to list agents")
		return
	}
	for _, name := range names {
		endpoint := h.endpoint(r, name)
		entries = append(entries, entry{Name: name, URL: endpoint, AgentCardURL: endpoint + a2asrv.WellKnownAgentCardPath})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"agents": entries})
}
func (h *a2aRoutes) card(w http.ResponseWriter, r *http.Request) {
	agent, ok := lookupAgent(w, r, h.registry)
	if !ok {
		return
	}
	card := h.agentCard(r, agent)
	a2asrv.NewStaticAgentCardHandler(card).ServeHTTP(w, r)
}
func (h *a2aRoutes) agentCard(r *http.Request, agent *agents.Agent) *a2a.AgentCard {
	return &a2a.AgentCard{
		Name: agent.Name, Description: "Interact with " + agent.Name + " through HasteKit.", Version: "1.0.0",
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(h.endpoint(r, agent.Name), a2a.TransportProtocolJSONRPC)},
		DefaultInputModes:   []string{"text/plain", "application/json"}, DefaultOutputModes: []string{"text/plain", "application/json"},
		Capabilities: a2a.AgentCapabilities{Streaming: true},
		Skills:       []a2a.AgentSkill{{ID: agent.Name, Name: agent.Name, Description: "Run " + agent.Name + " with its configured tools and skills.", Tags: []string{"agent"}}},
	}
}
func (h *a2aRoutes) invoke(w http.ResponseWriter, r *http.Request) {
	agent, ok := lookupAgent(w, r, h.registry)
	if !ok {
		return
	}
	key := a2aKey{agent.Name, requestNamespace(r)}
	h.mu.Lock()
	cached := h.adapters[key]
	adapter := cached.adapter
	if adapter == nil || cached.agent != agent {
		opts := []agents.A2AOption{agents.WithA2ANamespace(key.namespace), agents.WithA2AAuthorizer(h.options.a2aAuthorizer)}
		if h.options.a2aHandlerOptions != nil {
			opts = append(opts, agents.WithA2AHandlerOptions(h.options.a2aHandlerOptions(agent.Name, key.namespace)...))
		}
		adapter = agent.A2A(h.agentCard(r, agent), opts...)
		h.adapters[key] = a2aAdapter{agent: agent, adapter: adapter}
	}
	h.mu.Unlock()
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	adapter.InvokeHandler.ServeHTTP(w, r)
}
