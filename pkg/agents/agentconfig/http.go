package agentconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// ErrReserved reports a config whose name belongs to a built-in or code agent,
// which would shadow it so that it never ran.
var ErrReserved = errors.New("agent name is reserved")

// HandlerOption configures NewHandler.
type HandlerOption func(*handlerOptions)

type handlerOptions struct {
	namespace func(*http.Request) (string, error)
	validate  func(context.Context, Config) error
	choices   func(context.Context, string) (Choices, error)
}

// WithNamespaceResolver supplies the authenticated subject whose configs the
// routes manage. Never derive it from an untrusted query parameter or body.
func WithNamespaceResolver(resolve func(*http.Request) (string, error)) HandlerOption {
	return func(o *handlerOptions) { o.namespace = resolve }
}

// WithValidator checks each config before it is stored, beyond its own shape:
// pass Registry.Validate, which refuses reserved names and anything the
// Builder cannot run. Without one, only Validate's shape checks apply.
func WithValidator(validate func(context.Context, Config) error) HandlerOption {
	return func(o *handlerOptions) { o.validate = validate }
}

// WithChoices serves GET /agent-choices: what the requester's configs may
// pick from, for a UI building one. Pass Registry.Choices.
func WithChoices(choices func(ctx context.Context, namespace string) (Choices, error)) HandlerOption {
	return func(o *handlerOptions) { o.choices = choices }
}

// NewHandler exposes the requester's own agent configs:
//
//	GET    /agent-configs           → {"agents": [...], "nextCursor": "..."} (?limit, ?cursor)
//	POST   /agent-configs           → create or replace the config in the body
//	GET    /agent-configs/{name}    → one config
//	PUT    /agent-configs/{name}    → create or replace; the body's name must match or be empty
//	DELETE /agent-configs/{name}    → remove (idempotent)
//	GET    /agent-choices           → models, tools, skills, connectors and agents to pick (WithChoices)
//
// Built-in agents are never managed here: a namespace that resolves empty is
// refused, whatever the resolver says, so no request can write the empty
// namespace. Without a namespace resolver every request fails closed. It
// mirrors skills.NewHandler.
func NewHandler(store MutableStore, options ...HandlerOption) http.Handler {
	settings := handlerOptions{}
	for _, option := range options {
		if option != nil {
			option(&settings)
		}
	}
	mux := http.NewServeMux()
	route := func(pattern string, fn func(http.ResponseWriter, *http.Request, string)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			if settings.namespace == nil {
				http.Error(w, "namespace denied", http.StatusForbidden)
				return
			}
			ns, err := settings.namespace(r)
			if err != nil || strings.TrimSpace(ns) == "" {
				http.Error(w, "namespace denied", http.StatusForbidden)
				return
			}
			if store == nil {
				http.Error(w, "agent config store unavailable", http.StatusServiceUnavailable)
				return
			}
			fn(w, r, ns)
		})
	}

	route("GET /agent-configs", func(w http.ResponseWriter, r *http.Request, ns string) {
		n := 0
		if v := r.URL.Query().Get("limit"); v != "" {
			var err error
			n, err = strconv.Atoi(v)
			if err != nil || n < 1 || n > 200 {
				fail(w, fmt.Errorf("%w: limit must be 1–200", ErrInvalid))
				return
			}
		}
		page, err := store.List(r.Context(), ns, ListOptions{Limit: n, Cursor: r.URL.Query().Get("cursor")})
		if err != nil {
			fail(w, err)
			return
		}
		if page.Agents == nil {
			page.Agents = []Config{}
		}
		respond(w, http.StatusOK, page)
	})

	save := func(w http.ResponseWriter, r *http.Request, ns string) {
		cfg, err := readConfig(w, r)
		if err != nil {
			fail(w, err)
			return
		}
		if name := r.PathValue("name"); name != "" {
			if cfg.Name == "" {
				cfg.Name = name
			}
			if cfg.Name != name {
				fail(w, fmt.Errorf("%w: route and agent name differ", ErrInvalid))
				return
			}
		}
		if err := Validate(cfg); err != nil {
			fail(w, err)
			return
		}
		if settings.validate != nil {
			if err := settings.validate(r.Context(), cfg); err != nil {
				fail(w, err)
				return
			}
		}
		stored, err := store.Put(r.Context(), ns, cfg)
		if err != nil {
			fail(w, err)
			return
		}
		respond(w, http.StatusOK, stored)
	}
	route("POST /agent-configs", save)
	route("PUT /agent-configs/{name}", save)

	route("GET /agent-choices", func(w http.ResponseWriter, r *http.Request, ns string) {
		if settings.choices == nil {
			fail(w, ErrNotFound)
			return
		}
		choices, err := settings.choices(r.Context(), ns)
		if err != nil {
			fail(w, err)
			return
		}
		respond(w, http.StatusOK, choices)
	})
	route("GET /agent-configs/{name}", func(w http.ResponseWriter, r *http.Request, ns string) {
		cfg, err := store.Get(r.Context(), ns, r.PathValue("name"))
		if err != nil {
			fail(w, err)
			return
		}
		respond(w, http.StatusOK, cfg)
	})
	route("DELETE /agent-configs/{name}", func(w http.ResponseWriter, r *http.Request, ns string) {
		if err := store.Delete(r.Context(), ns, r.PathValue("name")); err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func readConfig(w http.ResponseWriter, r *http.Request) (Config, error) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxConfigBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return Config{}, fmt.Errorf("%w: config exceeds %d bytes", ErrInvalid, MaxConfigBytes)
		}
		return Config{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("%w: one JSON object expected", ErrInvalid)
	}
	return cfg, nil
}

// respond writes one of the handler's JSON bodies.
func respond[T Page | Config | Choices](w http.ResponseWriter, status int, v T) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	message := "agent config operation failed"
	switch {
	case errors.Is(err, ErrInvalid):
		status, message = http.StatusBadRequest, err.Error()
	case errors.Is(err, ErrNotFound):
		status, message = http.StatusNotFound, err.Error()
	case errors.Is(err, ErrReserved):
		status, message = http.StatusConflict, err.Error()
	case errors.Is(err, ErrReadOnly):
		status, message = http.StatusForbidden, err.Error()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
