package agui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents/skills"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/stretchr/testify/require"
)

func TestSkillSelectionInput(t *testing.T) {
	for _, raw := range []string{`{"skills":{"enable":["review"],"disable":["default"]}}`, `{"skills":{"enable":13}}`} {
		var fp map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &fp))
		in := RunAgentInput{ForwardedProps: fp}
		selection, err := in.SkillSelection()
		if raw == `{"skills":{"enable":13}}` {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
			require.Equal(t, []string{"review"}, selection.Enable)
			require.Equal(t, []string{"default"}, selection.Disable)
		}
	}
}

type skillRegistryForHTTP struct{ agent *agents.Agent }

func (s skillRegistryForHTTP) AgentNames() []string { return []string{"helper"} }
func (s skillRegistryForHTTP) Agent(name string) (*agents.Agent, bool) {
	return s.agent, name == "helper"
}

func TestSkillsEndpointNamespaceAndPolicies(t *testing.T) {
	set := &httpSkillSet{t: t}
	agent := agents.NewAgent(&agents.AgentOptions{Name: "helper", Skills: []agents.SkillSet{set}})
	handler := NewHandler(skillRegistryForHTTP{agent}, WithNamespaceResolver(func(*http.Request) (string, error) { return "tenant", nil }))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/agents/helper/skills", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	var body struct {
		Skills []agents.ListedSkill `json:"skills"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Len(t, body.Skills, 2)
	require.True(t, body.Skills[0].Enabled)
	require.False(t, body.Skills[1].Enabled)
	require.NotContains(t, recorder.Body.String(), "secret")
}

func TestSkillStoreManagementUsesResolvedNamespace(t *testing.T) {
	store, err := skills.NewFileStore(t.TempDir())
	require.NoError(t, err)
	source, err := skills.NewSkillSet("library", store)
	require.NoError(t, err)
	agent := agents.NewAgent(&agents.AgentOptions{Name: "helper", Skills: []agents.SkillSet{source}})
	handler := NewHandler(skillRegistryForHTTP{agent}, WithSkillStore(store), WithNamespaceResolver(func(r *http.Request) (string, error) { return r.Header.Get("Tenant"), nil }))
	data, err := json.Marshal(skills.Bundle{Files: map[string][]byte{"SKILL.md": []byte("---\nname: review\ndescription: Review work\n---\nInstructions")}})
	require.NoError(t, err)
	req := httptest.NewRequest("POST", "/skills?namespace=other", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Tenant", "one")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	require.Equal(t, 200, recorder.Code, recorder.Body.String())
	for _, tenant := range []string{"one", "two"} {
		req := httptest.NewRequest("GET", "/agents/helper/skills", nil)
		req.Header.Set("Tenant", tenant)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		require.Equal(t, 200, recorder.Code)
		var body struct {
			Skills []agents.ListedSkill `json:"skills"`
		}
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
		if tenant == "one" {
			require.Len(t, body.Skills, 1)
			require.Equal(t, "review", body.Skills[0].Name)
			require.False(t, body.Skills[0].Enabled)
		} else {
			require.Empty(t, body.Skills)
		}
	}
	_, err = store.Get(context.Background(), "other", "review")
	require.ErrorIs(t, err, skills.ErrNotFound)
}

func TestGlobalSkillsRemainReadOnlyThroughTenantManagement(t *testing.T) {
	store, err := skills.NewFileStore(t.TempDir())
	require.NoError(t, err)
	global := skills.Bundle{Files: map[string][]byte{"SKILL.md": []byte("---\nname: review\ndescription: Shared review\n---\nGlobal instructions")}}
	_, err = store.Put(t.Context(), "global", global)
	require.NoError(t, err)
	source, err := skills.NewSkillSet("library", store, skills.WithGlobalNamespace("global"))
	require.NoError(t, err)
	agent := agents.NewAgent(&agents.AgentOptions{Name: "helper", Skills: []agents.SkillSet{source}})
	handler := NewHandler(skillRegistryForHTTP{agent}, WithSkillStore(store), WithNamespaceResolver(func(*http.Request) (string, error) { return "tenant", nil }))
	local := skills.Bundle{Files: map[string][]byte{"SKILL.md": []byte("---\nname: review\ndescription: Tenant review\n---\nTenant instructions")}}
	data, err := json.Marshal(local)
	require.NoError(t, err)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		path := "/skills/review?namespace=global"
		if method == http.MethodPost {
			path = "/skills?namespace=global"
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		require.Less(t, recorder.Code, 300, recorder.Body.String())
		stored, err := store.Get(t.Context(), "global", "review")
		require.NoError(t, err)
		require.Equal(t, global, stored)
		tenant, err := store.Get(t.Context(), "tenant", "review")
		if method == http.MethodDelete {
			require.ErrorIs(t, err, skills.ErrNotFound)
		} else {
			require.NoError(t, err)
			require.Equal(t, local, tenant)
		}
	}
	// The picker still exposes shared skills after the tenant copy is deleted.
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/agents/helper/skills", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), "Shared review")
	// The management library lists only the tenant's own uploads.
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/skills?namespace=global", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.NotContains(t, recorder.Body.String(), "review")
}

type httpSkillSet struct {
	t *testing.T
}

func (s *httpSkillSet) GetName() string { return "team" }

func (s *httpSkillSet) ListSkills(ctx context.Context, namespace string, rc map[string]any) ([]agents.Skill, error) {
	require.Equal(s.t, "tenant", namespace)
	require.NotContains(s.t, rc, "Namespace")
	return []agents.Skill{{Name: "required", Required: true}, {Name: "optional"}}, nil
}

func (s *httpSkillSet) ResolveSkill(context.Context, string, map[string]any, string, string) (string, error) {
	panic("unexpected skill read")
}
