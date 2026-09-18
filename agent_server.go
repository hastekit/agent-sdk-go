package sdk

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/hastekit/agent-sdk-go/pkg/workflow"
)

type HTTPHandler struct{ registry *AgentRegistry }

func NewHTTPHandler(registry *AgentRegistry) *HTTPHandler {
	return &HTTPHandler{registry: registry}
}

func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	agentName := r.URL.Query().Get("agent")
	if agentName == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	agent, exists := h.registry.Agent(agentName)
	if !exists {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	var payload agents.AgentInput
	if err := utils.DecodeJSON(r.Body, &payload); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	handle, err := agent.Execute(r.Context(), &payload)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	for chunk := range handle.Chunks {
		buf, err := sonic.Marshal(chunk)
		if err != nil {
			continue
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", chunk.ChunkType(), buf); err != nil {
			return
		}
		flusher.Flush()
	}

	if _, err := handle.Wait(r.Context()); err != nil {
		// Transport failures (including an unread event backlog) may have no
		// corresponding event from the agent itself. Surface them over SSE.
		if r.Context().Err() == nil {
			chunk := responses.NewStreamError(err)
			if buf, marshalErr := sonic.Marshal(chunk); marshalErr == nil {
				_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", chunk.ChunkType(), buf)
				flusher.Flush()
			}
		}
		return
	}
}

// ErrAgentAlreadyRegistered indicates a duplicate name in one registry.
var ErrAgentAlreadyRegistered = errors.New("agent already registered")

// ErrWorkflowAlreadyRegistered indicates a duplicate workflow name in a registry.
var ErrWorkflowAlreadyRegistered = workflow.ErrAlreadyRegistered

// AgentRegistry is an instance-owned, concurrency-safe registry of agents and
// workflows. Its zero value is ready to use. Names and registered graphs must
// not be mutated after registration.
type AgentRegistry struct {
	mu        sync.RWMutex
	agents    map[string]*agents.Agent
	workflows workflow.Registry
}

func NewRegistry() *AgentRegistry { return &AgentRegistry{} }

func (r *AgentRegistry) Register(agent *Agent) error {
	if r == nil {
		return fmt.Errorf("nil registry")
	}
	if agent == nil || agent.Name == "" {
		return fmt.Errorf("agent and name are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.agents == nil {
		r.agents = make(map[string]*agents.Agent)
	}
	if _, exists := r.agents[agent.Name]; exists {
		return fmt.Errorf("%w: %s", ErrAgentAlreadyRegistered, agent.Name)
	}
	r.agents[agent.Name] = agent
	return nil
}

func (r *AgentRegistry) Agent(name string) (*agents.Agent, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	agent, ok := r.agents[name]
	return agent, ok
}

func (r *AgentRegistry) AgentNames() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.agents))
	for name := range r.agents {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// RegisterWorkflow registers an independently invokable workflow alongside agents.
func (r *AgentRegistry) RegisterWorkflow(name string, compiled *workflow.Compiled, options ...workflow.InvokeOption) error {
	if r == nil {
		return fmt.Errorf("nil registry")
	}
	return r.workflows.Register(name, compiled, options...)
}
func (r *AgentRegistry) Workflow(name string) (*workflow.Compiled, bool) {
	if r == nil {
		return nil, false
	}
	return r.workflows.Workflow(name)
}
func (r *AgentRegistry) WorkflowNames() []string {
	if r == nil {
		return nil
	}
	return r.workflows.WorkflowNames()
}

// RunWorkflow executes or resumes a workflow by its registered name.
func (r *AgentRegistry) RunWorkflow(ctx context.Context, name string, in *workflow.Input, options ...workflow.InvokeOption) (*workflow.Input, error) {
	if r == nil {
		return in, fmt.Errorf("nil registry")
	}
	return r.workflows.Execute(ctx, name, in, options...)
}

// NewWorkflowHTTPHandler serves independent workflow start/status/resume routes.
// Keep the returned handler for the server's lifetime: it owns bounded, in-memory
// HTTP run checkpoints. It does not require an agent or model.
func NewWorkflowHTTPHandler(registry *AgentRegistry, config workflow.HTTPConfig) *workflow.HTTPHandler {
	var workflows *workflow.Registry
	if registry != nil {
		workflows = &registry.workflows
	}
	return workflow.NewHTTPHandler(workflows, config)
}
