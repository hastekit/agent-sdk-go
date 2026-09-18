package routines

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

type HTTPConfig struct {
	// Scheduler optionally exposes GET /routines/{id}/status.
	Scheduler Scheduler
	// Resolve the authenticated tenant. Empty/nil uses default (single tenant).
	NamespaceResolver func(*http.Request) (string, error)
	MaxBodyBytes      int64
}

// NewHTTPHandler exposes /routines and /routines/{id}, plus POST
// /routines/{id}/pause and /resume, and GET /routines/agents. Mount behind the
// application's authentication middleware. A namespace is not authentication.
func NewHTTPHandler(service *Service, config HTTPConfig) http.Handler {
	if config.MaxBodyBytes <= 0 {
		config.MaxBodyBytes = 1 << 20
	}
	mux := http.NewServeMux()
	wrap := func(fn func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			ns := "default"
			if config.NamespaceResolver != nil {
				value, err := config.NamespaceResolver(r)
				if err != nil {
					writeJSON(w, 403, map[string]string{"error": "namespace access denied"})
					return
				}
				ns = namespace(value)
			}
			fn(w, r, ns)
		}
	}
	decode := func(w http.ResponseWriter, r *http.Request, d any) bool {
		r.Body = http.MaxBytesReader(w, r.Body, config.MaxBodyBytes)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(d); err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid request body"})
			return false
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			writeJSON(w, 400, map[string]string{"error": "expected one JSON object"})
			return false
		}
		return true
	}
	mux.HandleFunc("GET /routines", wrap(func(w http.ResponseWriter, r *http.Request, ns string) {
		v, err := service.List(r.Context(), ns)
		respond(w, 200, v, err)
	}))
	mux.HandleFunc("GET /routines/agents", wrap(func(w http.ResponseWriter, r *http.Request, ns string) {
		names := []string{}
		if registry, ok := service.executor.(interface{ AgentNames() []string }); ok {
			names = append(names, registry.AgentNames()...)
		}
		writeJSON(w, 200, names)
	}))
	mux.HandleFunc("POST /routines", wrap(func(w http.ResponseWriter, r *http.Request, ns string) {
		var d Definition
		if !decode(w, r, &d) {
			return
		}
		v, err := service.Create(r.Context(), ns, d)
		respond(w, 201, v, err)
	}))
	if config.Scheduler != nil {
		mux.HandleFunc("GET /routines/{id}/status", wrap(func(w http.ResponseWriter, r *http.Request, ns string) {
			if _, err := service.Get(r.Context(), ns, r.PathValue("id")); err != nil {
				respond(w, 200, nil, err)
				return
			}
			state, err := config.Scheduler.Status(r.Context(), ns, r.PathValue("id"))
			respond(w, 200, state, err)
		}))
	}
	mux.HandleFunc("GET /routines/{id}", wrap(func(w http.ResponseWriter, r *http.Request, ns string) {
		v, err := service.Get(r.Context(), ns, r.PathValue("id"))
		respond(w, 200, v, err)
	}))
	mux.HandleFunc("PUT /routines/{id}", wrap(func(w http.ResponseWriter, r *http.Request, ns string) {
		var d Definition
		if !decode(w, r, &d) {
			return
		}
		v, err := service.Update(r.Context(), ns, r.PathValue("id"), d)
		respond(w, 200, v, err)
	}))
	mux.HandleFunc("DELETE /routines/{id}", wrap(func(w http.ResponseWriter, r *http.Request, ns string) {
		err := service.Delete(r.Context(), ns, r.PathValue("id"))
		respond(w, 204, nil, err)
	}))
	for _, action := range []string{"pause", "resume"} {
		mux.HandleFunc("POST /routines/{id}/"+action, wrap(func(w http.ResponseWriter, r *http.Request, ns string) {
			v, err := service.SetEnabled(r.Context(), ns, r.PathValue("id"), action == "resume")
			respond(w, 200, v, err)
		}))
	}
	return mux
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if status != 204 {
		_ = json.NewEncoder(w).Encode(v)
	}
}
func respond(w http.ResponseWriter, status int, v any, err error) {
	if err != nil {
		status = 500
		message := "routines storage unavailable"
		switch {
		case errors.Is(err, ErrNotFound):
			status = 404
			message = err.Error()
		case errors.Is(err, ErrInvalid):
			status = 400
			message = err.Error()
		case errors.Is(err, ErrConflict):
			status = 409
			message = err.Error()
		}
		writeJSON(w, status, map[string]string{"error": message})
		return
	}
	writeJSON(w, status, v)
}
