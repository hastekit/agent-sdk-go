package restate_agent_builder

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentconfig"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/restate_runtime"
	"github.com/hastekit/agent-sdk-go/pkg/gateway"
	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Runtime is the caller side: an agentconfig.Runtime that invokes the
// AgentBuilder workflow through the Restate ingress.
type Runtime struct {
	client *ingress.Client
	broker agents.StreamBroker
}

var _ agentconfig.Runtime = (*Runtime)(nil)

// NewRuntime invokes workflows through the ingress at endpoint. broker must
// be the one the service publishes on.
func NewRuntime(endpoint string, broker agents.StreamBroker) *Runtime {
	client := ingress.NewClient(endpoint, restate.WithHttpClient(&http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}))
	return &Runtime{client: client, broker: broker}
}

// StreamBroker is the broker the workflows publish on.
func (r *Runtime) StreamBroker() agents.StreamBroker { return r.broker }

// Bind returns the runtime that runs snapshot.
func (r *Runtime) Bind(snapshot *agentconfig.Snapshot) agents.Runtime {
	return &snapshotRun{runtime: r, snapshot: snapshot}
}

// snapshotRun is one config-built agent's runtime.
type snapshotRun struct {
	runtime  *Runtime
	snapshot *agentconfig.Snapshot
}

func (s *snapshotRun) StreamBroker() agents.StreamBroker        { return s.runtime.broker }
func (s *snapshotRun) RegisterAgent(*agents.AgentOptions) error { return nil }

// Run invokes the workflow under a key of its own. A Restate workflow key runs
// once, and a thread's stream id is the same on every turn, so the stream id
// travels in the input instead of naming the invocation.
func (s *snapshotRun) Run(ctx context.Context, _ *agents.Agent, in *agents.AgentInput) (*agents.AgentOutput, error) {
	if in.StreamID == "" {
		in.StreamID = uuid.NewString()
	}
	input := &Input{
		Snapshot: s.snapshot,
		WorkflowInput: restate_runtime.WorkflowInput{
			AgentName:         s.snapshot.Root,
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
			StreamID:          in.StreamID,
			ProviderConfigKey: gateway.ProviderConfigKeyFromContext(ctx),
		},
	}
	return ingress.Workflow[*Input, *agents.AgentOutput](s.runtime.client, ServiceName, uuid.NewString(), "Run").Request(ctx, input)
}
