package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type registry map[string]*agents.Agent

func (r registry) Agent(name string) (*agents.Agent, bool) {
	a, ok := r[name]
	return a, ok
}

func (r registry) AgentNames() []string {
	names := make([]string, 0, len(r))
	for name := range r {
		names = append(names, name)
	}
	return names
}

func TestHandlerServesEmbeddedUIAndAPI(t *testing.T) {
	agent := agents.NewAgent(&agents.AgentOptions{Name: "Helper"})
	server := httptest.NewServer(Handler(registry{"Helper": agent}))
	defer server.Close()

	// Embedded UI at the root.
	res, err := http.Get(server.URL + "/")
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	assert.True(t, strings.Contains(string(body), "HasteKit"), "embedded index.html should render")

	// Protocol endpoints under /api/agui.
	res, err = http.Get(server.URL + APIPrefix + "/agents")
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)
	var payload struct {
		Agents []string `json:"agents"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&payload))
	assert.Equal(t, []string{"Helper"}, payload.Agents)

	// Thread listing reachable through the same mount.
	res, err = http.Get(server.URL + APIPrefix + "/agents/Helper/threads")
	require.NoError(t, err)
	defer res.Body.Close()
	assert.Equal(t, http.StatusOK, res.StatusCode)
}

// The UI bundle is built from ui/src and committed, so source and
// artefact can drift apart silently — a `go build` never notices. This
// asserts the bundle that actually ships still carries the stop wiring:
// the CUSTOM event it reads the stream id from, and a call to the stop
// endpoint. Rebuild with `pnpm build` in ui/ if it fails.
func TestEmbeddedBundleCallsStopEndpoint(t *testing.T) {
	agent := agents.NewAgent(&agents.AgentOptions{Name: "Helper"})
	server := httptest.NewServer(Handler(registry{"Helper": agent}))
	defer server.Close()

	res, err := http.Get(server.URL + "/")
	require.NoError(t, err)
	defer res.Body.Close()
	index, err := io.ReadAll(res.Body)
	require.NoError(t, err)

	// Follow the module script tag the page actually loads.
	_, after, found := strings.Cut(string(index), `src="./assets/`)
	require.True(t, found, "index.html should load a bundle from assets/")
	asset, _, found := strings.Cut(after, `"`)
	require.True(t, found)

	res, err = http.Get(server.URL + "/assets/" + asset)
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)
	bundle, err := io.ReadAll(res.Body)
	require.NoError(t, err)

	assert.Contains(t, string(bundle), "hastekit.stream_id",
		"bundle should read the run's stream id from the CUSTOM event")
	assert.Contains(t, string(bundle), "/stop",
		"bundle should call the stop endpoint rather than only aborting the stream")
}
