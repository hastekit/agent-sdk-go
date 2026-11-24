package routines

import (
	"context"
	"errors"
	"fmt"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

var ErrPaused = errors.New("agent paused awaiting input; resume through the application's agent API")

// AgentRegistry finds the agents a namespace can run, so a routine can target
// its namespace's own agents as well as shared ones. It has the shape of
// agui.Registry: agentconfig.Registry, which builds agents namespaces create
// at run time, satisfies both. Wrap agents built in code with StaticRegistry.
type AgentRegistry interface {
	// Agent returns the agent namespace runs under name. ok is false when the
	// namespace has no such agent; an error means the lookup itself failed.
	Agent(ctx context.Context, namespace, name string) (agent *agents.Agent, ok bool, err error)
	// AgentNames lists the agents namespace can run.
	AgentNames(ctx context.Context, namespace string) ([]string, error)
}

// StaticAgents is a fixed set of agents built in code, such as
// *sdk.AgentRegistry.
type StaticAgents interface {
	Agent(name string) (*agents.Agent, bool)
	AgentNames() []string
}

// StaticRegistry offers the same agents to every namespace.
func StaticRegistry(set StaticAgents) AgentRegistry { return staticRegistry{set: set} }

type staticRegistry struct{ set StaticAgents }

func (s staticRegistry) Agent(_ context.Context, _ string, name string) (*agents.Agent, bool, error) {
	if s.set == nil {
		return nil, false, nil
	}
	agent, ok := s.set.Agent(name)
	return agent, ok, nil
}

func (s staticRegistry) AgentNames(context.Context, string) ([]string, error) {
	if s.set == nil {
		return nil, nil
	}
	return s.set.AgentNames(), nil
}

// AgentLister is an Executor that can list the agents a namespace's routines
// may target, for the routes and tools that offer them.
type AgentLister interface {
	AgentNames(ctx context.Context, namespace string) ([]string, error)
}

// AgentExecutor dispatches to registered agents using their configured runtime.
// Each occurrence gets an isolated thread/session. ContextResolver optionally
// loads trusted credentials/context at execution time, never from tool arguments.
// Agents are looked up in the routine's own namespace.
type AgentExecutor struct {
	Registry        AgentRegistry
	ContextResolver func(context.Context, Routine) (map[string]any, error)
}

var _ AgentLister = (*AgentExecutor)(nil)

func (e *AgentExecutor) HasAgent(ctx context.Context, namespace, name string) (bool, error) {
	if e == nil || e.Registry == nil {
		return false, nil
	}
	_, ok, err := e.Registry.Agent(ctx, namespace, name)
	return ok, err
}
func (e *AgentExecutor) AgentNames(ctx context.Context, namespace string) ([]string, error) {
	if e == nil || e.Registry == nil {
		return nil, nil
	}
	return e.Registry.AgentNames(ctx, namespace)
}
func (e *AgentExecutor) Execute(ctx context.Context, r Routine, run Run) error {
	if e == nil || e.Registry == nil {
		return errors.New("no agent registry")
	}
	agent, ok, err := e.Registry.Agent(ctx, r.Namespace, r.Agent)
	if err != nil {
		return fmt.Errorf("load agent %q: %w", r.Agent, err)
	}
	if !ok {
		return fmt.Errorf("agent %q no longer available", r.Agent)
	}
	var runContext map[string]any
	if e.ContextResolver != nil {
		var err error
		runContext, err = e.ContextResolver(ctx, r)
		if err != nil {
			return err
		}
	}
	// Copy resolver output before adding scheduler-owned metadata.
	trusted := map[string]any{}
	for k, v := range runContext {
		trusted[k] = v
	}
	trusted[history.RoutineIDContextKey] = r.ID
	trusted[history.RoutineAgentContextKey] = r.Agent
	trusted["routine_occurrence_id"] = run.ID
	result, err := agent.Run(ctx, &agents.AgentInput{
		GroupID: r.ID, Namespace: r.Namespace, ThreadID: run.ID, SessionID: run.ID, RunID: run.AgentRunID,
		Message:    history.Message{ID: run.ID, SenderID: "routine:" + r.ID, Messages: []responses.InputMessageUnion{responses.UserMessage(r.Instruction)}},
		RunContext: trusted,
	})
	if err != nil {
		return err
	}
	if result == nil {
		return errors.New("agent returned no result")
	}
	if result.Status == agentstate.RunStatusPaused {
		return ErrPaused
	}
	if result.Status != agentstate.RunStatusCompleted {
		return fmt.Errorf("agent finished with status %s", result.Status)
	}
	return nil
}
