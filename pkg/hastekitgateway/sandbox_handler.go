package hastekitgateway

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
	"net/http"
)

// NewSandboxHandler serves HasteKit Gateway sandbox routes relative to /.
// Mount with http.StripPrefix("/api/sandbox", handler).
// The host must apply authentication and namespace authorization middleware.
func NewSandboxHandler(provider sandbox.Provider) http.Handler {
	mux := http.NewServeMux()
	fail := func(w http.ResponseWriter, err error) {
		status := http.StatusBadGateway
		switch {
		case errors.Is(err, sandbox.ErrNotFound):
			status = 404
		case errors.Is(err, sandbox.ErrConflict):
			status = 409
		case errors.Is(err, sandbox.ErrUnsupported):
			status = 501
		case errors.Is(err, context.DeadlineExceeded):
			status = 504
		}
		http.Error(w, err.Error(), status)
	}
	ref := func(r *http.Request) sandbox.Reference {
		return sandbox.Reference{Provider: r.PathValue("provider"), ID: r.PathValue("id")}
	}
	respond := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("POST /{$}", func(w http.ResponseWriter, r *http.Request) {
		var req sandbox.CreateRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		s, err := provider.Create(r.Context(), req)
		if err != nil {
			fail(w, err)
			return
		}
		respond(w, s.Reference())
	})
	mux.HandleFunc("GET /{provider}/{id}", func(w http.ResponseWriter, r *http.Request) {
		s, err := provider.Connect(r.Context(), ref(r))
		if err != nil {
			fail(w, err)
			return
		}
		respond(w, s.Reference())
	})
	mux.HandleFunc("DELETE /{provider}/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := provider.Delete(r.Context(), ref(r)); err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(204)
	})
	resolve := func(r *http.Request) (sandbox.Sandbox, error) {
		sb, err := provider.Connect(r.Context(), ref(r))
		if err != nil {
			return nil, err
		}
		if id := r.Header.Get("X-Sandbox-ID"); id != "" && (sb.Reference().ID != id || sb.Reference().Provider != r.Header.Get("X-Sandbox-Provider")) {
			return nil, sandbox.ErrConflict
		}
		return sb, nil
	}
	mux.HandleFunc("POST /{provider}/{id}/v2/exec", func(w http.ResponseWriter, r *http.Request) {
		var req sandbox.ExecRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if _, err := sandbox.ValidateExec(req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		s, err := resolve(r)
		if err != nil {
			fail(w, err)
			return
		}
		result, err := s.Exec(r.Context(), req)
		if err != nil {
			fail(w, err)
			return
		}
		respond(w, result)
	})
	return mux
}
