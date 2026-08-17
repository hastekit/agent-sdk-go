package sdk

import (
	"context"
	"reflect"
	"runtime"
	"strings"

	json "github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

type Tool = agents.Tool

// ToolAnnotations describes what a tool does — read-only, destructive,
// idempotent, open-world — in the same shape MCP servers use, so a function
// tool and an MCP tool feed the same permission policy.
type ToolAnnotations = agents.ToolAnnotations

type FunctionTool[T any, S any] struct {
	name          string
	description   string
	needsApproval bool
	deferred      bool
	annotations   *agents.ToolAnnotations
	fn            ToolFunc[T, S]
}

func (t *FunctionTool[T, S]) SetName(name string) {
	t.name = name
}

func (t *FunctionTool[T, S]) SetDescription(description string) {
	t.description = description
}

func (t *FunctionTool[T, S]) SetNeedsApproval(needsApproval bool) {
	t.needsApproval = needsApproval
}

func (t *FunctionTool[T, S]) SetDeferred(deferred bool) {
	t.deferred = deferred
}

// SetAnnotations replaces the tool's annotations wholesale. The hint options
// (WithReadOnly, WithDestructive, ...) go through here too, each filling in one
// field of the current set.
func (t *FunctionTool[T, S]) SetAnnotations(annotations *agents.ToolAnnotations) {
	t.annotations = annotations
}

// GetAnnotations implements agents.AnnotatedTool.
func (t *FunctionTool[T, S]) GetAnnotations() *agents.ToolAnnotations {
	return t.annotations
}

type ToolConfig interface {
	SetName(string)
	SetDescription(string)
	SetNeedsApproval(bool)
	SetDeferred(bool)
	SetAnnotations(*agents.ToolAnnotations)
	// GetAnnotations lets the per-hint options amend the current annotations
	// instead of overwriting them, so WithReadOnly and WithIdempotent can be
	// passed to the same tool.
	GetAnnotations() *agents.ToolAnnotations
}

type ToolFunc[T any, S any] func(ctx context.Context, in T) (S, error)

func NewTool[T any, S any](fn ToolFunc[T, S], opts ...ToolOption) *FunctionTool[T, S] {
	fnVal := reflect.ValueOf(fn)

	ft := &FunctionTool[T, S]{
		name: strings.SplitN(runtime.FuncForPC(fnVal.Pointer()).Name(), ".", 2)[1],
		fn:   fn,
	}

	for _, opt := range opts {
		opt(ft)
	}

	return ft
}

type ToolOption func(ToolConfig)

func WithName(name string) ToolOption {
	return func(cfg ToolConfig) {
		cfg.SetName(name)
	}
}

func WithDescription(desc string) ToolOption {
	return func(ft ToolConfig) {
		ft.SetDescription(desc)
	}
}

func WithNeedsApproval(needsApproval bool) ToolOption {
	return func(ft ToolConfig) {
		ft.SetNeedsApproval(needsApproval)
	}
}

func WithDeferred(deferred bool) ToolOption {
	return func(ft ToolConfig) {
		ft.SetDeferred(deferred)
	}
}

// WithAnnotations sets the tool's behavioural hints in one go. Prefer the
// single-hint options below unless you already have a full set in hand.
func WithAnnotations(annotations *agents.ToolAnnotations) ToolOption {
	return func(ft ToolConfig) {
		ft.SetAnnotations(annotations)
	}
}

// WithReadOnly declares that the tool does not modify anything. This is what a
// permission policy keys off to let a call run unattended, so only claim it
// when the tool truly has no side effects.
func WithReadOnly(readOnly bool) ToolOption {
	return annotate(func(a *agents.ToolAnnotations) { a.ReadOnlyHint = &readOnly })
}

// WithDestructive declares whether the tool may destroy or overwrite state, as
// opposed to only adding to it. A tool that says nothing counts as destructive.
func WithDestructive(destructive bool) ToolOption {
	return annotate(func(a *agents.ToolAnnotations) { a.DestructiveHint = &destructive })
}

// WithIdempotent declares that repeating the call with the same arguments has
// no additional effect.
func WithIdempotent(idempotent bool) ToolOption {
	return annotate(func(a *agents.ToolAnnotations) { a.IdempotentHint = &idempotent })
}

// WithOpenWorld declares whether the tool reaches outside a closed, known set
// of entities — a web search does, a lookup in local memory does not.
func WithOpenWorld(openWorld bool) ToolOption {
	return annotate(func(a *agents.ToolAnnotations) { a.OpenWorldHint = &openWorld })
}

// WithTitle sets a human-readable name for the tool, for UI use. The model
// still sees the tool by its name.
func WithTitle(title string) ToolOption {
	return annotate(func(a *agents.ToolAnnotations) { a.Title = title })
}

// annotate applies one hint to the tool's annotations, creating the set on
// first use so the hint options compose rather than clobber each other. It
// works on a copy: a set handed to several tools via WithAnnotations must not
// pick up one tool's hints on another.
func annotate(set func(*agents.ToolAnnotations)) ToolOption {
	return func(ft ToolConfig) {
		annotations := &agents.ToolAnnotations{}
		if current := ft.GetAnnotations(); current != nil {
			*annotations = *current
		}
		set(annotations)
		ft.SetAnnotations(annotations)
	}
}

func (t *FunctionTool[T, S]) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	var in T
	err := json.Unmarshal([]byte(params.Arguments), &in)
	if err != nil {
		return nil, err
	}

	out, err := t.fn(ctx, in)
	if err != nil {
		return nil, err
	}

	s, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}

	return &agents.ToolCallResponse{
		FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			ID:     params.ID,
			CallID: params.CallID,
			Output: responses.FunctionCallOutputContentUnion{
				OfString: utils.Ptr(string(s)),
			},
		},
	}, nil
}

func (t *FunctionTool[T, S]) Tool(ctx context.Context) *responses.ToolUnion {
	var in T

	return &responses.ToolUnion{
		OfFunction: &responses.FunctionTool{
			Name:        t.name,
			Description: utils.Ptr(t.description),
			Parameters:  NewOutputSchema(in),
			Strict:      utils.Ptr(false),
		},
	}
}

func (t *FunctionTool[T, S]) NeedApproval() bool {
	return t.needsApproval
}

func (t *FunctionTool[T, S]) IsDeferred() bool {
	return t.deferred
}
