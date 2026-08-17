package sdk

import (
	"fmt"
	"net/http"
	"sort"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

type HTTPHandler struct{}

func NewHTTPHandler() *HTTPHandler {
	return &HTTPHandler{}
}

func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	agentName := r.URL.Query().Get("agent")
	if agentName == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	agent, exists := agentsByName[agentName]
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
		_, _ = fmt.Fprintf(w, "event: %s\n", chunk.ChunkType())
		_, _ = fmt.Fprintf(w, "data: %s\n\n", buf)
		flusher.Flush()
	}

	if _, err := handle.Wait(); err != nil {
		// Agent already streamed any error chunks before exit; nothing
		// useful to do at this point because headers are flushed.
		return
	}
}

type AgentRegistry struct {
}

// Agent returns a registered agent by name.
func (_ *AgentRegistry) Agent(name string) (*agents.Agent, bool) {
	agent, ok := agentsByName[name]
	return agent, ok
}

// AgentNames returns the names of all registered agents, sorted.
func (_ *AgentRegistry) AgentNames() []string {
	names := make([]string, 0, len(agentsByName))
	for name := range agentsByName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
