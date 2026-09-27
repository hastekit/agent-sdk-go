package agui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/skills"
	"github.com/stretchr/testify/require"
)

func TestSkillSelectionInput(t *testing.T) {
	// A legacy enable list is ignored: user skills are on unless disabled.
	for _, raw := range []string{`{"skills":{"enable":["review"],"disable":["draft"]}}`, `{"skills":{"disable":13}}`} {
		var fp map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &fp))
		in := RunAgentInput{ForwardedProps: fp}
		selection, err := in.SkillSelection()
		if raw == `{"skills":{"disable":13}}` {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
			require.Equal(t, []string{"draft"}, selection.Disable)
		}
	}
}

// skillAgents is a registry of named agents for HTTP tests.
type skillAgents map[string]*agents.Agent

func (s skillAgents) AgentNames() []string {
	var names []string
	for name := range s {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
func (s skillAgents) Agent(name string) (*agents.Agent, bool) {
	agent, ok := s[name]
	return agent, ok
}

func skillBundle(name, description string) skills.Bundle {
	return skills.Bundle{Files: map[string][]byte{"SKILL.md": []byte("---\nname: " + name + "\ndescription: " + description + "\n---\nInstructions")}}
}

func TestSkillsAreNotScopedToAgents(t *testing.T) {
	store, err := skills.NewFileStore(t.TempDir())
	require.NoError(t, err)
	agent := agents.NewAgent(&agents.AgentOptions{Name: "helper", SkillClient: skills.NewClient(store)})
	handler := NewHandler(skillAgents{"helper": agent}, WithSkillStore(store), WithNamespaceResolver(func(*http.Request) (string, error) { return "tenant", nil }))

	// The per-agent catalog route is gone; users manage one library for every agent.
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/agents/helper/skills", nil))
	require.Equal(t, http.StatusNotFound, recorder.Code)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/skills", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.JSONEq(t, `{"skills":[]}`, recorder.Body.String())
}

func TestSkillStoreManagementUsesResolvedNamespace(t *testing.T) {
	store, err := skills.NewFileStore(t.TempDir())
	require.NoError(t, err)
	agent := agents.NewAgent(&agents.AgentOptions{Name: "helper", SkillClient: skills.NewClient(store)})
	handler := NewHandler(skillAgents{"helper": agent}, WithSkillStore(store), WithNamespaceResolver(func(r *http.Request) (string, error) { return r.Header.Get("Tenant"), nil }))
	data, err := json.Marshal(skillBundle("review", "Review work"))
	require.NoError(t, err)
	req := httptest.NewRequest("POST", "/skills?namespace=other", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Tenant", "one")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	require.Equal(t, 200, recorder.Code, recorder.Body.String())
	for _, tenant := range []string{"one", "two"} {
		req := httptest.NewRequest("GET", "/skills", nil)
		req.Header.Set("Tenant", tenant)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		require.Equal(t, 200, recorder.Code)
		var body skills.Page
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
		catalog, err := agent.ListSkills(t.Context(), tenant, nil, agents.SkillSelection{})
		require.NoError(t, err)
		if tenant == "one" {
			require.Len(t, body.Skills, 1)
			require.Equal(t, "review", body.Skills[0].Name)
			require.Len(t, catalog, 1)
			require.True(t, catalog[0].Enabled, "the agent reads the same library")
		} else {
			require.Empty(t, body.Skills)
			require.Empty(t, catalog)
		}
	}
	_, err = store.Get(context.Background(), "other", "review")
	require.ErrorIs(t, err, skills.ErrNotFound)
}

func TestUploadsCannotReuseAnyAgentsGlobalSkillName(t *testing.T) {
	store, err := skills.NewFileStore(t.TempDir())
	require.NoError(t, err)
	base := skills.NewClient(store)
	reviewGlobals, err := skills.NewBundleSource(skillBundle("review", "Developer review"))
	require.NoError(t, err)
	styleGlobals, err := skills.NewBundleSource(skillBundle("style", "Developer style"))
	require.NoError(t, err)
	registry := skillAgents{
		"reviewer": agents.NewAgent(&agents.AgentOptions{Name: "reviewer", SkillClient: base.WithGlobalSkills(reviewGlobals)}),
		"writer":   agents.NewAgent(&agents.AgentOptions{Name: "writer", SkillClient: base.WithGlobalSkills(styleGlobals)}),
	}
	handler := NewHandler(registry, WithSkillStore(store), WithNamespaceResolver(func(*http.Request) (string, error) { return "tenant", nil }))
	upload := func(method, path, name string) int {
		data, err := json.Marshal(skillBundle(name, "Tenant copy"))
		require.NoError(t, err)
		req := httptest.NewRequest(method, path, bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		return recorder.Code
	}

	// Either agent's global names are reserved for every upload route.
	require.Equal(t, http.StatusConflict, upload(http.MethodPost, "/skills", "review"))
	require.Equal(t, http.StatusConflict, upload(http.MethodPut, "/skills/style", "style"))
	require.Equal(t, http.StatusOK, upload(http.MethodPost, "/skills", "mine"))
	for _, name := range []string{"review", "style"} {
		_, err := store.Get(t.Context(), "tenant", name)
		require.ErrorIs(t, err, skills.ErrNotFound)
	}

	// Each agent keeps its own globals and reads the tenant's skill; the library lists only uploads.
	for name, global := range map[string]string{"reviewer": "review", "writer": "style"} {
		catalog, err := registry[name].ListSkills(t.Context(), "tenant", nil, agents.SkillSelection{})
		require.NoError(t, err)
		require.Len(t, catalog, 2)
		require.Equal(t, global, catalog[0].Name)
		require.True(t, catalog[0].Global)
		require.Equal(t, "mine", catalog[1].Name)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/skills", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), "mine")
	require.NotContains(t, recorder.Body.String(), "review")
	require.NotContains(t, recorder.Body.String(), "style")
}
