package agents

import (
	"context"
	"fmt"
	"maps"
	"slices"
)

// SubAgentQuery says who is asking a SubAgentClient, and for which run. It is
// plain data, so a durable runtime can carry it into the activity or step that
// lists the catalog.
type SubAgentQuery struct {
	// Caller is the name of the agent whose run is asking. A client that lets
	// an agent call itself lists it under this name.
	Caller     string         `json:"caller"`
	Namespace  string         `json:"namespace"`
	RunContext map[string]any `json:"run_context,omitempty"`
}

// SubAgentInfo is one agent a caller may hand work to, as the model sees it.
type SubAgentInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`

	// Self marks the caller itself: a call to it hands the work to a copy of
	// the caller, working in a thread of its own.
	Self bool `json:"self,omitempty"`
}

// SubAgentRequest is one message for a sub-agent.
type SubAgentRequest struct {
	// Caller is the agent whose tool sent the message. A client that lets an
	// agent call itself runs a call to Caller.Name on Caller, as it is — with
	// its model, whether or not it was registered anywhere.
	Caller *Agent

	// Name is the sub-agent the message is for, as ListSubAgents named it.
	Name string

	// Input is the sub-agent's run. Input.Message.ID is fixed by the call, so
	// a retried call carries the same one; Input.StreamID is the sub-agent
	// thread's channel (see StreamIDForThread).
	Input *AgentInput
}

// SubAgentClient owns an agent's sub-agents: which agents it may hand work to,
// and how a message reaches one. It mirrors SkillClient and MCPClient, and
// AgentOptions.SubAgents gives an agent the call_sub_agent tool that uses it.
//
// ListSubAgents is the catalog the model is shown, and the allowlist the tool
// checks a call against; RunSubAgent and SteerSubAgent must refuse a name it
// would not list. Names are unique.
//
// RunSubAgent delivers Input to the named agent as RunAgentTask does, and must
// be just as safe to retry: a message already on the thread is not run again.
// SteerSubAgent adds Input.Message to the run going on the sub-agent's thread,
// answering false when none is going there.
//
// Implementations must be safe for concurrent use. See subagents.Client for
// one backed by an agent registry.
type SubAgentClient interface {
	ListSubAgents(ctx context.Context, query SubAgentQuery) ([]SubAgentInfo, error)
	RunSubAgent(ctx context.Context, req SubAgentRequest) (AgentTaskOutcome, error)
	SteerSubAgent(ctx context.Context, req SubAgentRequest) (bool, error)
}

// Description is what the agent is for, as a caller that can hand it work is
// told — see AgentOptions.Description.
func (e *Agent) Description() string {
	return e.description
}

// listSubAgentCatalog lists the caller's sub-agents for a run, checking the
// client's answer: the listing is the allowlist call_sub_agent checks against,
// so anything ambiguous is refused.
func listSubAgentCatalog(ctx context.Context, client SubAgentClient, query SubAgentQuery) ([]SubAgentInfo, error) {
	listed, err := client.ListSubAgents(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list sub-agents: %w", err)
	}
	seen := map[string]bool{}
	for _, info := range listed {
		if info.Name == "" {
			return nil, fmt.Errorf("sub-agent with no name")
		}
		if seen[info.Name] {
			return nil, fmt.Errorf("duplicate sub-agent %q", info.Name)
		}
		seen[info.Name] = true
	}
	return listed, nil
}

// subAgentQuery is the query a run of this agent lists its sub-agents with.
func (e *Agent) subAgentQuery(in *AgentInput) SubAgentQuery {
	runContext := maps.Clone(in.RunContext)
	if runContext == nil {
		runContext = map[string]any{}
	}
	return SubAgentQuery{Caller: e.Name, Namespace: in.Namespace, RunContext: runContext}
}

// prepareSubAgents lists the run's sub-agents, for the prompt. With none to
// call, the run is not offered call_sub_agent.
func (e *Agent) prepareSubAgents(ctx context.Context, in *AgentInput, tools []Tool) ([]Tool, []SubAgentInfo, error) {
	if e.subAgentClient == nil {
		return tools, nil, nil
	}
	listed, err := listSubAgentCatalog(ctx, e.subAgentClient, e.subAgentQuery(in))
	if err != nil {
		return nil, nil, err
	}
	if len(listed) == 0 {
		return slices.DeleteFunc(slices.Clone(tools), func(tool Tool) bool { return functionName(tool) == CallSubAgentToolName }), nil, nil
	}
	return tools, listed, nil
}

// SteerLocalSubAgent adds req's message to the run going on agent's thread, as
// a user's message joins a busy conversation: the run takes it at its next
// step. It answers false when no run is going there. It is what a
// SubAgentClient serving in-process agents does for SteerSubAgent, as
// RunAgentTask is for RunSubAgent.
//
// A run that ends between the check and the enqueue leaves the message on the
// thread's queue for its next turn rather than starting one on a guess.
func SteerLocalSubAgent(ctx context.Context, agent *Agent, req SubAgentRequest) (bool, error) {
	broker := agent.StreamBroker()
	if broker == nil {
		return false, nil
	}
	active, err := broker.IsActive(ctx, req.Input.StreamID)
	if err != nil {
		return false, fmt.Errorf("check %s on thread %s: %w", req.Name, req.Input.ThreadID, err)
	}
	if !active {
		return false, nil
	}
	if err := broker.EnqueueMessage(ctx, req.Input.StreamID, req.Input.Message); err != nil {
		return false, fmt.Errorf("send to %s on thread %s: %w", req.Name, req.Input.ThreadID, err)
	}
	return true, nil
}
