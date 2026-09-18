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

type AgentRegistry interface {
	Agent(string) (*agents.Agent, bool)
	AgentNames() []string
}

// AgentExecutor dispatches to registered agents using their configured runtime.
// Each occurrence gets an isolated thread/session. ContextResolver optionally
// loads trusted credentials/context at execution time, never from tool arguments.
type AgentExecutor struct {
	Registry        AgentRegistry
	ContextResolver func(context.Context, Routine) (map[string]any, error)
}

func (e *AgentExecutor) HasAgent(name string) bool {
	if e == nil || e.Registry == nil {
		return false
	}
	_, ok := e.Registry.Agent(name)
	return ok
}
func (e *AgentExecutor) AgentNames() []string {
	if e == nil || e.Registry == nil {
		return nil
	}
	return e.Registry.AgentNames()
}
func (e *AgentExecutor) Execute(ctx context.Context, r Routine, run Run) error {
	agent, ok := e.Registry.Agent(r.Agent)
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
