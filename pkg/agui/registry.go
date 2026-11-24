package agui

import (
	"context"
	"net/http"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
)

// GlobalSkillLister is an agents.Registry that can list every developer-owned
// global skill its agents use without building them. The /skills routes
// reserve these names, so a user's upload never sits shadowed by one. A
// registry without it is asked for each of its agents' skills instead.
type GlobalSkillLister interface {
	GlobalSkillNames(ctx context.Context) ([]string, error)
}

// lookupAgent resolves the {agent} path value in the request's namespace,
// answering the request itself when there is no agent to hand back.
func lookupAgent(w http.ResponseWriter, r *http.Request, registry agents.Registry) (*agents.Agent, bool) {
	agent, ok, err := registry.Agent(r.Context(), requestNamespace(r), r.PathValue("agent"))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "unable to load agent")
		return nil, false
	}
	if !ok || agent == nil {
		writeJSONError(w, http.StatusNotFound, "agent not found")
		return nil, false
	}
	return agent, true
}
