package agui

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
)

// FeedResponse is what the run feed answers with.
type FeedResponse struct {
	// Events are the runs that started or ended since the cursor, oldest
	// first. Empty when the wait ran out with nothing to report.
	Events []agents.RunEvent `json:"events"`

	// Cursor is where to resume. A client stores it and passes it back; it is
	// opaque and belongs to the server.
	Cursor string `json:"cursor"`
}

// serveRunFeed is the long poll a client sits on to learn that a run started
// or ended anywhere in the namespaces it cares about.
//
// This is the one a sidebar wants. The per-thread watch answers "is the
// conversation I am looking at busy"; this answers "did anything happen in any
// conversation" — including one that did not exist when the browser attached,
// which is precisely what a thread-keyed stream can never report.
//
// Namespaces come from the request so a caller minding several tenants can
// watch them in one connection. Absent, it watches the one the handler was
// built with, which is what an ordinary single-tenant UI wants.
func serveRunFeed(w http.ResponseWriter, r *http.Request, agent *agents.Agent, o options) {
	feed, ok := agent.StreamBroker().(agents.RunFeed)
	if !ok {
		writeJSONError(w, http.StatusNotImplemented, "the agent's stream broker does not publish a run feed")
		return
	}

	namespaces := feedNamespaces(r, o.namespace)
	if len(namespaces) == 0 {
		writeJSONError(w, http.StatusBadRequest, "at least one namespace is required")
		return
	}

	ctx := r.Context()
	events, cursor, err := feed.ReadRunEvents(
		ctx, namespaces, r.URL.Query().Get("cursor"), watchWait(r, defaultWatchWait))
	if err != nil {
		if ctx.Err() != nil {
			// The client went away mid-wait. Nothing to answer to.
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "unable to read the run feed: "+err.Error())
		return
	}

	if events == nil {
		events = []agents.RunEvent{}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(FeedResponse{Events: events, Cursor: cursor})
}

// feedNamespaces reads the namespaces to watch, falling back to the one the
// handler serves.
func feedNamespaces(r *http.Request, fallback string) []string {
	raw := r.URL.Query().Get("namespaces")
	if raw == "" {
		return []string{fallback}
	}

	seen := map[string]bool{}
	out := []string{}
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}
