package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// HTTPProvider exposes provider lifecycle and daemon operations through the gateway.
// Callers retain references in run state; the gateway holds no session bindings.
type HTTPProvider struct {
	BaseURL string
	Client  *http.Client
}

func (p *HTTPProvider) runtime(ctx context.Context, ref Reference, method string, input any) (Sandbox, error) {
	endpoint := strings.TrimRight(p.BaseURL, "/")
	if method != "POST" {
		if ref.ID == "" || ref.Provider == "" {
			return nil, fmt.Errorf("sandbox reference is required")
		}
		endpoint += "/" + url.PathEscape(ref.Provider) + "/" + url.PathEscape(ref.ID)
	}
	s, err := NewDaemonSandbox(DaemonConfig{Endpoint: endpoint, Client: p.Client, Reference: ref})
	if err != nil {
		return nil, err
	}
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	route := ""
	if method == "POST" {
		route = "/"
	}
	r, err := s.request(ctx, method, route, body, "application/json")
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	if method == "DELETE" {
		return nil, nil
	}
	if err := json.NewDecoder(r.Body).Decode(&s.ref); err != nil {
		return nil, err
	}
	if s.ref.ID == "" || s.ref.Provider == "" {
		return nil, fmt.Errorf("gateway returned an invalid sandbox reference")
	}
	if method == "POST" {
		s.endpoint += "/" + url.PathEscape(s.ref.Provider) + "/" + url.PathEscape(s.ref.ID)
	} else if s.ref != ref {
		return nil, ErrConflict
	}
	return &remoteSandbox{s}, nil
}
func (p *HTTPProvider) Create(ctx context.Context, req CreateRequest) (Sandbox, error) {
	return p.runtime(ctx, Reference{}, "POST", req)
}
func (p *HTTPProvider) Connect(ctx context.Context, ref Reference) (Sandbox, error) {
	return p.runtime(ctx, ref, "GET", nil)
}
func (p *HTTPProvider) Delete(ctx context.Context, ref Reference) error {
	_, err := p.runtime(ctx, ref, "DELETE", nil)
	return err
}

type remoteSandbox struct{ s *DaemonSandbox }

func (s *remoteSandbox) Reference() Reference { return s.s.Reference() }
func (s *remoteSandbox) Exec(ctx context.Context, req ExecRequest) (*ExecResult, error) {
	return s.s.Exec(ctx, req)
}
func (s *remoteSandbox) Files() FileSystem { return s.s.Files() }

// NewProviderHandler serves the reference API relative to /. Mount with StripPrefix.
// The host must apply authentication and namespace authorization middleware.
func NewProviderHandler(provider Provider) http.Handler {
	mux := http.NewServeMux()
	fail := func(w http.ResponseWriter, err error) {
		status := http.StatusBadGateway
		switch {
		case errors.Is(err, ErrNotFound):
			status = 404
		case errors.Is(err, ErrConflict):
			status = 409
		case errors.Is(err, ErrUnsupported):
			status = 501
		case errors.Is(err, context.DeadlineExceeded):
			status = 504
		}
		http.Error(w, err.Error(), status)
	}
	ref := func(r *http.Request) Reference {
		return Reference{Provider: r.PathValue("provider"), ID: r.PathValue("id")}
	}
	respond := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("POST /{$}", func(w http.ResponseWriter, r *http.Request) {
		var req CreateRequest
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
	resolve := func(r *http.Request) (Sandbox, error) {
		sb, err := provider.Connect(r.Context(), ref(r))
		if err != nil {
			return nil, err
		}
		if id := r.Header.Get("X-Sandbox-ID"); id != "" && (sb.Reference().ID != id || sb.Reference().Provider != r.Header.Get("X-Sandbox-Provider")) {
			return nil, ErrConflict
		}
		return sb, nil
	}
	mux.HandleFunc("POST /{provider}/{id}/v2/exec", func(w http.ResponseWriter, r *http.Request) {
		var req ExecRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if _, err := ValidateExec(req); err != nil {
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
	files := func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("path")
		if name == "" {
			http.Error(w, "path is required", 400)
			return
		}
		s, err := resolve(r)
		if err != nil {
			fail(w, err)
			return
		}
		switch r.Method {
		case "GET":
			reader, err := s.Files().Read(r.Context(), name)
			if err != nil {
				fail(w, err)
				return
			}
			defer reader.Close()
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = io.Copy(w, reader)
		case "PUT":
			if err := s.Files().Write(r.Context(), name, r.Body); err != nil {
				fail(w, err)
				return
			}
			w.WriteHeader(204)
		case "DELETE":
			if err := s.Files().Remove(r.Context(), name); err != nil {
				fail(w, err)
				return
			}
			w.WriteHeader(204)
		}
	}
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		mux.HandleFunc(method+" /{provider}/{id}/v2/files", files)
	}
	return mux
}
