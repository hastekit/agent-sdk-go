package agents

import "context"

type Runtime interface {
	// RegisterAgent registers the resolved configuration before execution. NewAgent
	// calls it after validation. Runtimes that need no registration may return nil.
	RegisterAgent(options *AgentOptions) error
	// StreamBroker returns the broker shared by the runtime and its agents.
	StreamBroker() StreamBroker
	Run(ctx context.Context, agent *Agent, in *AgentInput) (*AgentOutput, error)
}
