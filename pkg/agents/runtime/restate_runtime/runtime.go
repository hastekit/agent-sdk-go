package restate_runtime

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/gateway"
	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// WorkflowInput is the input structure for the Restate workflow.
type WorkflowInput struct {
	GroupID        string `json:"group_id,omitempty"`
	ParentThreadID string `json:"parent_thread_id,omitempty"`
	Hidden         bool   `json:"hidden,omitempty"`
	MCP            agents.MCPSelection
	Skills         agents.SkillSelection
	ClientTools    []agents.ClientToolDefinition
	RunID          string
	AgentName      string `json:"agent_name"`

	Namespace     string
	ThreadID      string
	SessionID     string
	PreviousRunID string
	Message       history.Message
	RunContext    map[string]any

	// StreamID is the broker channel used for streaming chunks and for
	// stop signaling. The runtime sets it equal to the Restate workflow
	// key so the workflow and the caller agree on the channel.
	StreamID string

	// ProviderConfigKey is the gateway provider config key (a virtual key
	// or direct provider API key). Restate has no context propagator, so the
	// value the caller placed on the context via gateway.WithProviderConfigKey
	// is carried here across the durable boundary and re-established on the
	// context inside the workflow.
	ProviderConfigKey string
}

// RestateRuntime executes agents via Restate workflows for durability.
// It invokes a workflow whose service reconstructs the agent from its
// explicitly registered configuration for crash recovery.
type RestateRuntime struct {
	client *ingress.Client
	broker agents.StreamBroker
}

// NewRestateRuntime creates a new Restate runtime.
// The agent name is used to look up configuration registered by the service.
func NewRestateRuntime(endpoint string, broker agents.StreamBroker) *RestateRuntime {
	client := ingress.NewClient(endpoint, restate.WithHttpClient(&http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}))
	return &RestateRuntime{
		client: client,
		broker: broker,
	}
}

// Run invokes a workflow for an agent already registered with the service.
func (r *RestateRuntime) Run(ctx context.Context, agent *agents.Agent, in *agents.AgentInput) (*agents.AgentOutput, error) {
	if in.StreamID == "" {
		in.StreamID = uuid.NewString()
	}
	streamID := in.StreamID

	input := &WorkflowInput{
		AgentName:         agent.Name,
		RunID:             in.RunID,
		Namespace:         in.Namespace,
		GroupID:           in.GroupID,
		ParentThreadID:    in.ParentThreadID,
		Hidden:            in.Hidden,
		ThreadID:          in.ThreadID,
		SessionID:         in.SessionID,
		PreviousRunID:     in.PreviousRunID,
		Message:           in.Message,
		RunContext:        in.RunContext,
		Skills:            in.Skills,
		MCP:               in.MCP,
		ClientTools:       in.ClientTools,
		StreamID:          streamID,
		ProviderConfigKey: gateway.ProviderConfigKeyFromContext(ctx),
	}

	// The workflow is keyed by the run, not the stream. The stream id is the
	// thread's channel and the same for every turn on it, while a Restate
	// workflow key runs once: keyed by the stream, a thread's second turn fails
	// with 409 "the workflow method was already invoked". The run claim on the
	// stream already keeps one run per thread in flight.
	return ingress.Workflow[*WorkflowInput, *agents.AgentOutput](
		r.client,
		"AgentWorkflow",
		agents.ExecutionID(in),
		"Run",
	).Request(ctx, input)
}

// StreamBroker returns the broker configured for this runtime.
func (r *RestateRuntime) StreamBroker() agents.StreamBroker { return r.broker }

// RegisterAgent is a no-op for this invocation-only runtime. Worker configurations
// are registered separately; the top-level SDK runtime manages that registration.
func (r *RestateRuntime) RegisterAgent(options *agents.AgentOptions) error { return nil }
