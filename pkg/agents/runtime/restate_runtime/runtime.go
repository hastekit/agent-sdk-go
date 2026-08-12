package restate_runtime

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/history"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway"
	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// WorkflowInput is the input structure for the Restate workflow.
type WorkflowInput struct {
	AgentName string `json:"agent_name"`

	Namespace         string
	ThreadID          string
	PreviousMessageID string
	Message           history.Message
	RunContext        map[string]any

	// StreamID is the broker channel used for streaming chunks and for
	// stop signaling. The runtime sets it equal to the Restate workflow
	// key so the workflow and the caller agree on the channel.
	StreamID string

	// PermissionMode is the turn's tool gating. WorkflowInput is a projection
	// of AgentInput rather than the thing itself, so a field left out here is
	// a field the workflow's agent never sees — an unattended run would pause
	// on the far side. (The thread's standing per-tool decisions need no
	// carrying: the workflow reads them from the thread's own meta.)
	PermissionMode agents.PermissionMode

	// ProviderConfigKey is the gateway provider config key (a virtual key
	// or direct provider API key). Restate has no context propagator, so the
	// value the caller placed on the context via gateway.WithProviderConfigKey
	// is carried here across the durable boundary and re-established on the
	// context inside the workflow.
	ProviderConfigKey string
}

// RestateRuntime executes agents via Restate workflows for durability.
// It registers the agent in the global registry and invokes a Restate workflow
// that reconstructs the agent with RestateExecutor for crash recovery.
type RestateRuntime struct {
	client *ingress.Client
	broker agents.StreamBroker
}

// NewRestateRuntime creates a new Restate runtime.
// The agentName is used to look up the agent config inside the workflow.
func NewRestateRuntime(endpoint string, broker agents.StreamBroker) *RestateRuntime {
	client := ingress.NewClient(endpoint, restate.WithHttpClient(&http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}))
	return &RestateRuntime{
		client: client,
		broker: broker,
	}
}

// Run registers the agent in the global registry and invokes the Restate workflow.
func (r *RestateRuntime) Run(ctx context.Context, agent *agents.Agent, in *agents.AgentInput) (*agents.AgentOutput, error) {
	if in.StreamID == "" {
		in.StreamID = uuid.NewString()
	}
	streamID := in.StreamID

	input := &WorkflowInput{
		AgentName:         agent.Name,
		Namespace:         in.Namespace,
		ThreadID:          in.ThreadID,
		PreviousMessageID: in.PreviousMessageID,
		Message:           in.Message,
		RunContext:        in.RunContext,
		StreamID:          streamID,
		PermissionMode:    in.PermissionMode,
		ProviderConfigKey: gateway.ProviderConfigKeyFromContext(ctx),
	}

	return ingress.Workflow[*WorkflowInput, *agents.AgentOutput](
		r.client,
		"AgentWorkflow",
		streamID,
		"Run",
	).Request(ctx, input)
}
