package restate_runtime

import (
	"context"
	"maps"
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
	GroupID     string `json:"group_id,omitempty"`
	MCP         agents.MCPSelection
	Skills      agents.SkillSelection
	ClientTools []agents.ClientToolDefinition
	RunID       string
	AgentName   string `json:"agent_name"`

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

	// GatewayContext carries the caller's gateway.AddContext values into every step.
	GatewayContext map[string]string `json:"gateway_context,omitempty"`
}

// withGatewayContext puts the provider key and gateway values on ctx, so every run step sees them.
func withGatewayContext[T restate.Context](ctx T, providerConfigKey string, values map[string]string) T {
	if providerConfigKey == "" && len(values) == 0 {
		return ctx
	}
	var goCtx context.Context = ctx
	if providerConfigKey != "" {
		goCtx = gateway.WithProviderConfigKey(goCtx, providerConfigKey)
	}
	if len(values) > 0 {
		goCtx = gateway.AddContext(goCtx, maps.Clone(values))
	}
	return restate.WrapContext(ctx, goCtx)
}

// gatewayContextOf copies the gateway values on ctx, or returns nil when there are none.
func gatewayContextOf(ctx context.Context) map[string]string {
	if ctx == nil {
		return nil
	}
	values := gateway.GetContext(ctx)
	if len(values) == 0 {
		return nil
	}
	return maps.Clone(values)
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
		GatewayContext:    gatewayContextOf(ctx),
		AgentName:         agent.Name,
		RunID:             in.RunID,
		Namespace:         in.Namespace,
		GroupID:           in.GroupID,
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

	if agent.FactoryBuilt() {
		// Factory agents share one workflow service and pin the ref they start with.
		return ingress.Workflow[*FactoryWorkflowInput, *agents.AgentOutput](
			r.client, FactoryWorkflowServiceName, streamID, "Run",
		).Request(ctx, &FactoryWorkflowInput{Ref: agent.Ref(), WorkflowInput: *input})
	}

	return ingress.Workflow[*WorkflowInput, *agents.AgentOutput](
		r.client,
		"AgentWorkflow",
		streamID,
		"Run",
	).Request(ctx, input)
}

// StreamBroker returns the broker configured for this runtime.
func (r *RestateRuntime) StreamBroker() agents.StreamBroker { return r.broker }

// RegisterAgent is a no-op for this invocation-only runtime. Worker configurations
// are registered separately; the top-level SDK runtime manages that registration.
func (r *RestateRuntime) RegisterAgent(options *agents.AgentOptions) error { return nil }
