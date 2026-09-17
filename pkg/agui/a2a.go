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

type a2aKey struct {
	agent     *agents.Agent
	namespace string
}
type a2aRoutes struct {
	registry Registry
	options  options
	mu       sync.Mutex
	adapters map[a2aKey]*agents.A2A
}

func (o options) mountA2A(mux *http.ServeMux, registry Registry) {
	routes := &a2aRoutes{registry: registry, options: o, adapters: map[a2aKey]*agents.A2A{}}
	mux.Handle("GET /a2a/{$}", o.withNamespace(http.HandlerFunc(routes.directory)))
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
	for _, name := range h.registry.AgentNames() {
		endpoint := h.endpoint(r, name)
		entries = append(entries, entry{Name: name, URL: endpoint, AgentCardURL: endpoint + a2asrv.WellKnownAgentCardPath})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"agents": entries})
}
func (h *a2aRoutes) card(w http.ResponseWriter, r *http.Request) {
	agent, ok := h.registry.Agent(r.PathValue("agent"))
	if !ok {
		writeJSONError(w, http.StatusNotFound, "agent not found")
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
	agent, ok := h.registry.Agent(r.PathValue("agent"))
	if !ok {
		writeJSONError(w, http.StatusNotFound, "agent not found")
		return
	}
	key := a2aKey{agent, requestNamespace(r)}
	h.mu.Lock()
	adapter := h.adapters[key]
	if adapter == nil {
		opts := []agents.A2AOption{agents.WithA2ANamespace(key.namespace)}
		if h.options.a2aHandlerOptions != nil {
			opts = append(opts, agents.WithA2AHandlerOptions(h.options.a2aHandlerOptions(agent.Name, key.namespace)...))
		}
		adapter = agent.A2A(h.agentCard(r, agent), opts...)
		h.adapters[key] = adapter
	}
	h.mu.Unlock()
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	adapter.InvokeHandler.ServeHTTP(w, r)
}
