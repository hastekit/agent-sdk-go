package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type stateRuntime struct{ data []byte }

func (s *stateRuntime) Reference() Reference { return Reference{Provider: "test", ID: "runtime"} }
func (s *stateRuntime) Exec(context.Context, ExecRequest) (*ExecResult, error) {
	return &ExecResult{Stdout: "out", ExitCode: 7}, nil
}
func (s *stateRuntime) Files() FileSystem { return s }
func (s *stateRuntime) Read(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.data)), nil
}
func (s *stateRuntime) Write(_ context.Context, _ string, r io.Reader) error {
	var err error
	s.data, err = io.ReadAll(r)
	return err
}
func (s *stateRuntime) Remove(context.Context, string) error { s.data = nil; return nil }

type stateProvider struct {
	runtime *stateRuntime
	creates int
}

func (p *stateProvider) Create(context.Context, CreateRequest) (Sandbox, error) {
	p.creates++
	p.runtime = &stateRuntime{}
	return p.runtime, nil
}
func (p *stateProvider) Connect(_ context.Context, ref Reference) (Sandbox, error) {
	if p.runtime == nil || p.runtime.Reference() != ref {
		return nil, ErrNotFound
	}
	return p.runtime, nil
}
func (p *stateProvider) Delete(context.Context, Reference) error { p.runtime = nil; return nil }

type handlerTransport struct{ handler http.Handler }

func (t handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	t.handler.ServeHTTP(w, r)
	return w.Result(), nil
}

func TestConversationStateAndStatelessGateway(t *testing.T) {
	ctx := context.Background()
	backend := &stateProvider{}
	gateway := &HTTPProvider{BaseURL: "http://gateway.test", Client: &http.Client{Transport: handlerTransport{NewProviderHandler(backend)}}}
	key := SessionKey{Namespace: "project", SessionID: "session"}
	state := map[string]string{}
	req := CreateRequest{Session: key, Profile: "analysis", Env: map[string]string{"TOKEN": "secret"}}
	_, updates, err := Acquire(ctx, gateway, state, req)
	if err != nil {
		t.Fatal(err)
	}
	maps.Copy(state, updates)
	if strings.Contains(state[StateKey], "secret") {
		t.Fatal("environment credentials persisted")
	}
	if _, _, err := Acquire(ctx, gateway, state, req); err != nil {
		t.Fatal(err)
	}
	if backend.creates != 1 {
		t.Fatal("duplicate sandbox")
	}
	// A fresh gateway/client has no session map to reload.
	gateway = &HTTPProvider{BaseURL: "http://gateway.test", Client: &http.Client{Transport: handlerTransport{NewProviderHandler(backend)}}}
	s, _, err := Acquire(ctx, gateway, state, req)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.Exec(ctx, ExecRequest{Argv: []string{"true"}})
	if err != nil || result.ExitCode != 7 {
		t.Fatalf("%+v %v", result, err)
	}
	data := []byte{0, 255, 128, 10}
	if err := s.Files().Write(ctx, "a b.bin", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	r, err := s.Files().Read(ctx, "a b.bin")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if !bytes.Equal(got, data) {
		t.Fatal("binary file changed")
	}
	if err := s.Files().Remove(ctx, "a b.bin"); err != nil {
		t.Fatal(err)
	}
	if err := gateway.Delete(ctx, s.Reference()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Acquire(ctx, gateway, state, req); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if backend.creates != 1 {
		t.Fatal("missing sandbox replaced")
	}
	req.Profile = "different"
	if _, _, err := Acquire(ctx, gateway, state, req); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	// Corrupt authoritative state fails without provisioning.
	state[StateKey] = "broken"
	if _, _, err := Acquire(ctx, gateway, state, req); err == nil {
		t.Fatal("expected corrupt state")
	}
}
