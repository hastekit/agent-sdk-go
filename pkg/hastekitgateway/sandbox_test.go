package hastekitgateway

import (
	"context"
	"errors"
	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type stateRuntime struct{}

func (s *stateRuntime) Reference() sandbox.Reference {
	return sandbox.Reference{Provider: "test", ID: "namespace:pod:immutable-uid"}
}
func (s *stateRuntime) Exec(context.Context, sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	return &sandbox.ExecResult{Stdout: "out", ExitCode: 7}, nil
}

type stateProvider struct {
	runtime *stateRuntime
	creates int
}

func (p *stateProvider) Create(context.Context, sandbox.CreateRequest) (sandbox.Sandbox, error) {
	p.creates++
	p.runtime = &stateRuntime{}
	return p.runtime, nil
}
func (p *stateProvider) Connect(_ context.Context, ref sandbox.Reference) (sandbox.Sandbox, error) {
	if p.runtime == nil || p.runtime.Reference() != ref {
		return nil, sandbox.ErrNotFound
	}
	return p.runtime, nil
}
func (p *stateProvider) Delete(context.Context, sandbox.Reference) error { p.runtime = nil; return nil }

type handlerTransport struct{ handler http.Handler }

func (t handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	t.handler.ServeHTTP(w, r)
	return w.Result(), nil
}

func TestConversationStateAndStatelessGateway(t *testing.T) {
	ctx := context.Background()
	backend := &stateProvider{}
	gateway := newGatewayClient(backend)
	key := sandbox.SessionKey{Namespace: "project", SessionID: "session"}
	state := map[string]string{}
	req := sandbox.CreateRequest{Session: key, Profile: "analysis", Env: map[string]string{"TOKEN": "secret"}}
	_, updates, err := sandbox.Acquire(ctx, gateway, state, req)
	if err != nil {
		t.Fatal(err)
	}
	maps.Copy(state, updates)
	if strings.Contains(state[sandbox.StateKey], "secret") {
		t.Fatal("environment credentials persisted")
	}
	if _, _, err := sandbox.Acquire(ctx, gateway, state, req); err != nil {
		t.Fatal(err)
	}
	if backend.creates != 1 {
		t.Fatal("duplicate sandbox")
	}
	// A fresh gateway/client has no session map to reload.
	gateway = newGatewayClient(backend)
	s, _, err := sandbox.Acquire(ctx, gateway, state, req)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.Exec(ctx, sandbox.ExecRequest{Argv: []string{"true"}})
	if err != nil || result.ExitCode != 7 {
		t.Fatalf("%+v %v", result, err)
	}
	if err := gateway.Delete(ctx, s.Reference()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := sandbox.Acquire(ctx, gateway, state, req); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatal(err)
	}
	if backend.creates != 1 {
		t.Fatal("missing sandbox replaced")
	}
	req.Profile = "different"
	if _, _, err := sandbox.Acquire(ctx, gateway, state, req); !errors.Is(err, sandbox.ErrConflict) {
		t.Fatal(err)
	}
	// Corrupt authoritative state fails without provisioning.
	state[sandbox.StateKey] = "broken"
	if _, _, err := sandbox.Acquire(ctx, gateway, state, req); err == nil {
		t.Fatal("expected corrupt state")
	}
}

func newGatewayClient(provider sandbox.Provider) *SandboxClient {
	config := Config{Endpoint: "http://gateway.test", HttpClient: &http.Client{Transport: handlerTransport{http.StripPrefix("/api/sandbox", NewSandboxHandler(provider))}}}
	return config.NewSandboxClient()
}

type conflictProvider struct{ sandbox.Provider }

func (conflictProvider) Create(context.Context, sandbox.CreateRequest) (sandbox.Sandbox, error) {
	return nil, sandbox.ErrConflict
}
func TestGatewaySandboxConflictAndCapabilities(t *testing.T) {
	client := newGatewayClient(conflictProvider{})
	if _, err := client.Create(context.Background(), sandbox.CreateRequest{}); !errors.Is(err, sandbox.ErrConflict) {
		t.Fatal("gateway conflict classification lost", err)
	}
	backend := &stateProvider{}
	client = newGatewayClient(backend)
	sb, err := client.Create(context.Background(), sandbox.CreateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sb.(sandbox.EndpointResolver); ok {
		t.Fatal("gateway client exposes direct backend endpoints")
	}
}
