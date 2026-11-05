package sandbox

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"
)

type stateRuntime struct{}

func (s *stateRuntime) Reference() Reference { return Reference{Provider: "test", ID: "runtime"} }
func (s *stateRuntime) Exec(context.Context, ExecRequest) (*ExecResult, error) {
	return &ExecResult{Stdout: "out", ExitCode: 7}, nil
}

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

func TestConversationStateInProcessProvider(t *testing.T) {
	ctx := context.Background()
	backend := &stateProvider{}
	provider := backend
	key := SessionKey{Namespace: "project", SessionID: "session"}
	state := map[string]string{}
	req := CreateRequest{Session: key, Profile: "analysis", Env: map[string]string{"TOKEN": "secret"}}
	_, updates, err := Acquire(ctx, provider, state, req)
	if err != nil {
		t.Fatal(err)
	}
	maps.Copy(state, updates)
	if strings.Contains(state[StateKey], "secret") {
		t.Fatal("environment credentials persisted")
	}
	if _, _, err := Acquire(ctx, provider, state, req); err != nil {
		t.Fatal(err)
	}
	if backend.creates != 1 {
		t.Fatal("duplicate sandbox")
	}
	// Reuse is driven by the supplied state, not by a provider binding store.
	s, _, err := Acquire(ctx, provider, state, req)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.Exec(ctx, ExecRequest{Argv: []string{"true"}})
	if err != nil || result.ExitCode != 7 {
		t.Fatalf("%+v %v", result, err)
	}
	if err := provider.Delete(ctx, s.Reference()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Acquire(ctx, provider, state, req); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if backend.creates != 1 {
		t.Fatal("missing sandbox replaced")
	}
	req.Profile = "different"
	if _, _, err := Acquire(ctx, provider, state, req); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	// Corrupt authoritative state fails without provisioning.
	state[StateKey] = "broken"
	if _, _, err := Acquire(ctx, provider, state, req); err == nil {
		t.Fatal("expected corrupt state")
	}
}
