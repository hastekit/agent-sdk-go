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

// FactoryRuntime runs agents an AgentFactory builds, so one builder works on every runtime.
type FactoryRuntime interface {
	Runtime
	// RegisterAgentFactory installs the factory; durable runtimes need it before Serve.
	RegisterAgentFactory(factory AgentFactory) error
	// BuildAgent builds ref with the registered factory, bound to this runtime.
	BuildAgent(ctx context.Context, ref AgentRef) (*Agent, error)
}
