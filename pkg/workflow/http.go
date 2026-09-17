package workflow

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// HTTPConfig controls independently invoked workflows. NamespaceResolver must
// return the namespace authorized by the application; nil/empty uses "default".
// RunContextResolver supplies trusted context for MCP credentials/nested agents.
// Checkpoints are held by this handler instance, bounded by RunTTL and MaxRuns.
type HTTPConfig struct {
	NamespaceResolver  func(*http.Request) (string, error)
	RunContextResolver func(*http.Request) (map[string]any, error)
	MaxBodyBytes       int64         // zero: 1 MiB
	RunTTL             time.Duration // zero: one hour after the latest execution finishes
	MaxRuns            int           // zero: 1000; running executions are never evicted
}

// HTTPHandler exposes /workflows and /workflows/{name}/runs routes. It keeps
// checkpoints in memory; reuse one handler for all requests to those routes.
// Restarting the process loses runs. For durable persistence, use Registry.Execute
// with a persisted Input through an application-owned API.
type HTTPHandler struct {
	registry *Registry
	config   HTTPConfig
	mux      *http.ServeMux
	mu       sync.Mutex
	runs     map[string]*httpRun
}
type httpRun struct {
	mu                          sync.Mutex
	namespace, name, id, status string
	updated                     time.Time
	call                        agents.ToolCall
	tool                        *Tool
	result                      *agents.ToolCallResponse
}
type runResponse struct {
	RunID      string                `json:"run_id"`
	Workflow   string                `json:"workflow"`
	Status     string                `json:"status"`
	Output     any                   `json:"output,omitempty"`
	Interrupts []responses.Interrupt `json:"interrupts,omitempty"`
	Error      string                `json:"error,omitempty"`
}

func NewHTTPHandler(registry *Registry, config HTTPConfig) *HTTPHandler {
	if config.MaxBodyBytes <= 0 {
		config.MaxBodyBytes = 1 << 20
	}
	if config.RunTTL <= 0 {
		config.RunTTL = time.Hour
	}
	if config.MaxRuns <= 0 {
		config.MaxRuns = 1000
	}
	h := &HTTPHandler{registry: registry, config: config, mux: http.NewServeMux(), runs: map[string]*httpRun{}}
	h.mux.HandleFunc("GET /workflows", h.list)
	h.mux.HandleFunc("POST /workflows/{name}/runs", h.start)
	h.mux.HandleFunc("GET /workflows/{name}/runs/{runID}", h.get)
	h.mux.HandleFunc("POST /workflows/{name}/runs/{runID}/resume", h.resume)
	return h
}
func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func httpError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
func (h *HTTPHandler) namespace(w http.ResponseWriter, r *http.Request) (string, bool) {
	ns := "default"
	if h.config.NamespaceResolver != nil {
		value, err := h.config.NamespaceResolver(r)
		if err != nil {
			httpError(w, http.StatusForbidden, "namespace access denied")
			return "", false
		}
		if value != "" {
			ns = value
		}
	}
	return ns, true
}
func (h *HTTPHandler) decode(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, h.config.MaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		httpError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		httpError(w, http.StatusBadRequest, "expected one JSON object")
		return false
	}
	return true
}
func (h *HTTPHandler) list(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.namespace(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflows": h.registry.WorkflowNames()})
}
func (h *HTTPHandler) pruneLocked() {
	for id, run := range h.runs {
		run.mu.Lock()
		expired := run.status != "running" && time.Since(run.updated) > h.config.RunTTL
		run.mu.Unlock()
		if expired {
			delete(h.runs, id)
		}
	}
}
func (h *HTTPHandler) find(w http.ResponseWriter, r *http.Request) (*httpRun, bool) {
	ns, ok := h.namespace(w, r)
	if !ok {
		return nil, false
	}
	h.mu.Lock()
	h.pruneLocked()
	run := h.runs[r.PathValue("runID")]
	h.mu.Unlock()
	if run == nil || run.namespace != ns || run.name != r.PathValue("name") {
		httpError(w, http.StatusNotFound, "workflow run not found")
		return nil, false
	}
	return run, true
}
func (h *HTTPHandler) start(w http.ResponseWriter, r *http.Request) {
	ns, ok := h.namespace(w, r)
	if !ok {
		return
	}
	entry, ok := h.registry.lookup(r.PathValue("name"))
	if !ok {
		httpError(w, http.StatusNotFound, "workflow not found")
		return
	}
	var payload struct {
		Input map[string]any `json:"input"`
	}
	if !h.decode(w, r, &payload) {
		return
	}
	if payload.Input == nil {
		httpError(w, http.StatusBadRequest, "input must be an object")
		return
	}
	var runContext map[string]any
	if h.config.RunContextResolver != nil {
		var err error
		runContext, err = h.config.RunContextResolver(r)
		if err != nil {
			httpError(w, http.StatusForbidden, "run context access denied")
			return
		}
	}
	tool, err := NewTool(r.PathValue("name"), "", nil, entry.compiled, entry.options...)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "workflow configuration is invalid")
		return
	}
	args, err := json.Marshal(payload.Input)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid workflow input")
		return
	}
	id := uuid.NewString()
	run := &httpRun{namespace: ns, name: r.PathValue("name"), id: id, status: "running", updated: time.Now(), tool: tool, call: agents.ToolCall{FunctionCallMessage: &responses.FunctionCallMessage{ID: id, CallID: id, Name: r.PathValue("name"), Arguments: string(args)}, Namespace: ns, RunContext: runContext, State: map[string]string{}}}
	h.mu.Lock()
	h.pruneLocked()
	if len(h.runs) >= h.config.MaxRuns {
		h.mu.Unlock()
		httpError(w, http.StatusServiceUnavailable, "workflow run capacity reached")
		return
	}
	h.runs[id] = run
	h.mu.Unlock()
	h.execute(w, r, run, http.StatusCreated)
}
func (h *HTTPHandler) get(w http.ResponseWriter, r *http.Request) {
	run, ok := h.find(w, r)
	if !ok {
		return
	}
	run.mu.Lock()
	response := run.response()
	run.mu.Unlock()
	writeJSON(w, http.StatusOK, response)
}
func (h *HTTPHandler) resume(w http.ResponseWriter, r *http.Request) {
	run, ok := h.find(w, r)
	if !ok {
		return
	}
	var payload struct {
		Resolutions []responses.InterruptResolution `json:"resolutions"`
	}
	if !h.decode(w, r, &payload) {
		return
	}
	run.mu.Lock()
	if run.status != "paused" {
		run.mu.Unlock()
		httpError(w, http.StatusConflict, "workflow run is not paused")
		return
	}
	if err := validateResolutions(run.result.Interrupts, payload.Resolutions); err != nil {
		run.mu.Unlock()
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	run.call.ShouldResume = true
	run.call.ResumeMessages = []responses.InputMessageUnion{{OfFunctionCallInterruptResolution: &responses.FunctionCallInterruptResolutionMessage{Resolutions: payload.Resolutions}}}
	run.status = "running"
	run.mu.Unlock()
	h.execute(w, r, run, http.StatusOK)
}

// Only one execution owns call at a time; readers access the separate result.
func (h *HTTPHandler) execute(w http.ResponseWriter, r *http.Request, run *httpRun, status int) {
	result, err := run.tool.Execute(r.Context(), &run.call)
	run.mu.Lock()
	run.updated = time.Now()
	if err != nil {
		run.status = "failed"
		run.result = nil
		status = http.StatusInternalServerError
		slog.ErrorContext(r.Context(), "workflow execution failed", "workflow", run.name, "run_id", run.id, "error", err)
	} else {
		run.result = result
		run.status = "completed"
		for k, v := range result.StateUpdates {
			run.call.State[k] = v
		}
		if len(result.Interrupts) > 0 {
			run.status = "paused"
		}
	}
	response := run.response()
	run.mu.Unlock()
	w.Header().Set("X-Workflow-Run-ID", run.id)
	writeJSON(w, status, response)
}
func (run *httpRun) response() runResponse {
	result := runResponse{RunID: run.id, Workflow: run.name, Status: run.status}
	if run.status == "failed" {
		result.Error = "workflow execution failed"
	}
	if run.result != nil {
		if run.status == "paused" {
			result.Interrupts = run.result.Interrupts
		}
		if run.status == "completed" && run.result.FunctionCallOutputMessage != nil && run.result.Output.OfString != nil {
			_ = json.Unmarshal([]byte(*run.result.Output.OfString), &result.Output)
		}
	}
	return result
}
func validateResolutions(interrupts []responses.Interrupt, resolutions []responses.InterruptResolution) error {
	if len(resolutions) == 0 {
		return fmt.Errorf("resolutions are required")
	}
	expected := map[string]responses.Interrupt{}
	for _, intr := range interrupts {
		expected[intr.FunctionCallMessage.CallID] = intr
	}
	seen := map[string]bool{}
	for _, resolution := range resolutions {
		intr, ok := expected[resolution.CallID]
		if !ok || seen[resolution.CallID] {
			return fmt.Errorf("unknown or duplicate interrupt call_id")
		}
		seen[resolution.CallID] = true
		if resolution.Action != responses.InterruptActionApprove && resolution.Action != responses.InterruptActionReject {
			return fmt.Errorf("action must be approve or reject")
		}
		if resolution.Action == responses.InterruptActionApprove {
			for _, elicitation := range intr.Elicitations {
				if elicitation.RequestedSchema == nil {
					continue
				}
				var raw jsonschema.Schema
				if err := decodeValue(elicitation.RequestedSchema, &raw); err != nil {
					return fmt.Errorf("invalid form schema")
				}
				schema, err := raw.Resolve(nil)
				if err != nil {
					return fmt.Errorf("invalid form schema")
				}
				var content any
				if err := json.Unmarshal(resolution.Content, &content); err != nil {
					return fmt.Errorf("form content is required")
				}
				if err := schema.Validate(content); err != nil {
					return fmt.Errorf("invalid form content: %w", err)
				}
			}
		}
	}
	return nil
}
