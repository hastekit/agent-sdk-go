package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/attachments"
	"github.com/hastekit/agent-sdk-go/pkg/agents/skills"
	"github.com/hastekit/agent-sdk-go/pkg/agui"
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

func TestHandlerForwardsNamespaceResolverToAPIAndAttachments(t *testing.T) {
	store, err := attachments.NewFileStore(t.TempDir(), attachments.FileStoreConfig{})
	require.NoError(t, err)
	defer store.Close()
	calls := 0
	a := agents.NewAgent(&agents.AgentOptions{Name: "Helper"})
	h := Handler(registry{"Helper": a}, agui.WithAttachmentStore(store), agui.WithNamespaceResolver(func(r *http.Request) (string, error) {
		calls++
		return r.Header.Get("X-Test-Tenant"), nil
	}))
	req := httptest.NewRequest("GET", APIPrefix+"/agents/Helper/threads/shared/stream", nil)
	req.Header.Set("X-Test-Tenant", "tenant")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, 204, w.Code)
	require.Equal(t, agents.StreamIDForThread("tenant", "shared"), w.Header().Get("X-Stream-Id"))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", APIPrefix+"/attachments/missing", nil))
	require.Equal(t, 400, w.Code) // malformed file ID, after namespace resolution
	require.Equal(t, 2, calls)
	// Static assets do not need namespace resolution.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	require.Equal(t, 2, calls)
}

func TestHandlerSkillStore(t *testing.T) {
	store, err := skills.NewFileStore(t.TempDir())
	require.NoError(t, err)
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			var configured skills.Store
			if enabled {
				configured = store
			}
			h := Handler(registry{}, agui.WithSkillStore(configured))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", APIPrefix+"/agents", nil))
			require.Equal(t, http.StatusOK, w.Code)
			var capability struct {
				SkillStore bool `json:"skill_store"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &capability))
			require.Equal(t, enabled, capability.SkillStore)
			w = httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", APIPrefix+"/skills", nil))
			if enabled {
				require.Equal(t, http.StatusOK, w.Code)
			} else {
				require.Equal(t, http.StatusNotFound, w.Code)
			}
		})
	}
}

func TestAttachmentsOnlyUnderAPIPrefix(t *testing.T) {
	store, err := attachments.NewFileStore(t.TempDir(), attachments.FileStoreConfig{})
	require.NoError(t, err)
	defer store.Close()
	h := Handler(registry{}, agui.WithAttachmentStore(store), agui.WithNamespaceResolver(func(r *http.Request) (string, error) {
		return r.Header.Get("X-Tenant"), nil
	}))
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	file, err := form.CreateFormFile("file", "notes.txt")
	require.NoError(t, err)
	_, err = io.WriteString(file, "attachment content")
	require.NoError(t, err)
	require.NoError(t, form.WriteField("session_id", "thread"))
	require.NoError(t, form.Close())
	req := httptest.NewRequest(http.MethodPost, APIPrefix+"/attachments/", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("X-Tenant", "alice")
	uploaded := httptest.NewRecorder()
	h.ServeHTTP(uploaded, req)
	require.Equal(t, http.StatusCreated, uploaded.Code, uploaded.Body.String())
	var saved attachments.HTTPFile
	require.NoError(t, json.Unmarshal(uploaded.Body.Bytes(), &saved))
	require.Equal(t, APIPrefix+"/attachments/"+strings.TrimPrefix(saved.FileID, "attachment://"), saved.URL)
	require.Equal(t, saved.URL, uploaded.Header().Get("Location"))
	for _, tenant := range []string{"alice", "bob"} {
		req := httptest.NewRequest(http.MethodGet, saved.URL, nil)
		req.Header.Set("X-Tenant", tenant)
		downloaded := httptest.NewRecorder()
		h.ServeHTTP(downloaded, req)
		if tenant == "alice" {
			require.Equal(t, http.StatusOK, downloaded.Code)
			require.Equal(t, "attachment content", downloaded.Body.String())
		} else {
			require.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, downloaded.Code)
		}
	}
	for _, request := range []struct{ method, path string }{
		{http.MethodPost, "/attachments/"},
		{http.MethodGet, strings.TrimPrefix(saved.URL, APIPrefix)},
	} {
		result := httptest.NewRecorder()
		h.ServeHTTP(result, httptest.NewRequest(request.method, request.path, nil))
		require.Equal(t, http.StatusNotFound, result.Code)
	}
}
