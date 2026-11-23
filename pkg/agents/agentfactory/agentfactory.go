// Package agentfactory builds runnable agents from stored specs, for runtimes that run them in process.
package agentfactory

import (
	"context"
	"fmt"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/prompts"
)

// Instruction renders a spec's instruction with the default resolvers, as the gateway renders stored prompts.
func Instruction(text string) agents.SystemPromptProvider {
	return prompts.New(text, prompts.WithResolver(prompts.DefaultResolvers()...))
}

// MaxLoops is a spec's loop limit as AgentOptions takes it.
func MaxLoops(spec *agents.AgentSpec) *int {
	if spec.MaxLoops <= 0 {
		return nil
	}
	loops := spec.MaxLoops
	return &loops
}

// Build loads ref and builds it. configure sets runtime options on it and on every handoff target.
func Build(ctx context.Context, factory *agents.AgentFactory, ref agents.AgentRef, configure func(*agents.AgentOptions)) (*agents.Agent, error) {
	spec, err := factory.LoadSpec(ctx, ref)
	if err != nil {
		return nil, err
	}
	return New(factory, spec, configure)
}

// New builds spec with the factory's models, tools and history. Handoff targets load when used.
func New(factory *agents.AgentFactory, spec *agents.AgentSpec, configure func(*agents.AgentOptions)) (*agents.Agent, error) {
	model, err := factory.Models.ResolveModel(spec.Model)
	if err != nil {
		return nil, fmt.Errorf("agent %s: model %q: %w", spec.Ref(), spec.Model, err)
	}
	tools := make([]agents.Tool, 0, len(spec.Tools))
	for _, name := range spec.Tools {
		tool := factory.Tool(name)
		if tool == nil {
			return nil, fmt.Errorf("agent %s: unknown tool %q", spec.Ref(), name)
		}
		tools = append(tools, tool)
	}
	handoffs := make([]*agents.Handoff, 0, len(spec.Handoffs))
	for _, h := range spec.Handoffs {
		target := &handoff{factory: factory, ref: h.Ref(), configure: configure}
		handoffs = append(handoffs, &agents.Handoff{Name: h.Name, Description: h.Description, Agent: target})
	}

	options := &agents.AgentOptions{
		Name:        spec.Name,
		Spec:        spec,
		LLM:         model,
		Parameters:  spec.Parameters,
		Output:      spec.Output,
		MaxLoops:    MaxLoops(spec),
		Tools:       tools,
		Handoffs:    handoffs,
		History:     factory.History,
		Middlewares: factory.Middlewares,
	}
	if spec.Instruction != "" {
		options.Instruction = Instruction(spec.Instruction)
	}
	if configure != nil {
		configure(options)
	}
	return agents.NewAgent(options), nil
}

// handoff loads its target when the model hands off, so specs may reference each other in cycles.
type handoff struct {
	factory   *agents.AgentFactory
	ref       agents.AgentRef
	configure func(*agents.AgentOptions)
}

func (h *handoff) ExecuteWithRun(ctx context.Context, in *agents.AgentInput, run *history.ConversationRunManager) (*agents.AgentOutput, error) {
	target, err := Build(ctx, h.factory, h.ref, h.configure)
	if err != nil {
		return &agents.AgentOutput{Status: agentstate.RunStatusError, RunID: run.GetRunID()}, fmt.Errorf("handoff to %s: %w", h.ref, err)
	}
	return target.ExecuteWithRun(ctx, in, run)
}
