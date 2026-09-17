package workflow

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

var (
	ErrAlreadyRegistered = errors.New("workflow already registered")
	ErrWorkflowNotFound  = errors.New("workflow not found")
)

type registration struct {
	compiled *Compiled
	options  []InvokeOption
}

// Registry holds named, compiled workflows. Its zero value is ready to use.
// Registered graphs must not be mutated. Execution state belongs to the caller,
// so independent runs can execute concurrently and paused state can be persisted.
type Registry struct {
	mu        sync.RWMutex
	workflows map[string]registration
}

func NewRegistry() *Registry { return &Registry{} }

func (r *Registry) Register(name string, compiled *Compiled, options ...InvokeOption) error {
	if r == nil || strings.TrimSpace(name) == "" || compiled == nil {
		return fmt.Errorf("workflow registry, name and compiled graph are required")
	}
	for _, option := range options {
		if option == nil {
			return fmt.Errorf("nil workflow invocation option")
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.workflows[name]; exists {
		return fmt.Errorf("%w: %s", ErrAlreadyRegistered, name)
	}
	if r.workflows == nil {
		r.workflows = map[string]registration{}
	}
	r.workflows[name] = registration{compiled: compiled, options: append([]InvokeOption(nil), options...)}
	return nil
}
func (r *Registry) lookup(name string) (registration, bool) {
	if r == nil {
		return registration{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.workflows[name]
	return entry, ok
}
func (r *Registry) Workflow(name string) (*Compiled, bool) {
	entry, ok := r.lookup(name)
	return entry.compiled, ok
}
func (r *Registry) WorkflowNames() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.workflows))
	for name := range r.workflows {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Execute invokes a workflow by name, using the options supplied at registration.
// Resume a paused run by calling Input.SetResume and passing that same state again.
// Callers must not execute the same Input concurrently.
func (r *Registry) Execute(ctx context.Context, name string, in *Input, options ...InvokeOption) (*Input, error) {
	entry, ok := r.lookup(name)
	if !ok {
		return in, fmt.Errorf("%w: %s", ErrWorkflowNotFound, name)
	}
	all := append([]InvokeOption(nil), entry.options...)
	all = append(all, options...)
	for _, option := range all {
		if option == nil {
			return in, fmt.Errorf("nil workflow invocation option")
		}
	}
	return entry.compiled.Execute(ctx, in, all...)
}
