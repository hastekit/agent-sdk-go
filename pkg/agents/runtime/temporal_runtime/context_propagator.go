package temporal_runtime

import (
	"context"

	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/workflow"

	"github.com/hastekit/agent-sdk-go/pkg/gateway"
)

// providerConfigKeyHeader is the Temporal header slot that carries the
// gateway provider config key across the workflow and activity boundaries.
const providerConfigKeyHeader = "hastekit-provider-config-key"

// workflowProviderConfigKey is the key under which the value is stored on a
// workflow.Context. The context.Context side reuses the gateway helpers so the
// value lands where the LLM client's getKey reads it.
type workflowProviderConfigKey struct{}

// ProviderConfigKeyPropagator moves the gateway provider config key
// (set by gateway.WithProviderConfigKey on the caller's context) across the
// Temporal client -> workflow -> activity boundary.
//
// A plain context.Context value cannot cross these boundaries on its own: the
// value has to be carried in Temporal headers and re-established on the far
// side. Registering this propagator on both the client and the worker makes the
// key transparently available inside every activity's context.Context — which
// is exactly where the LLM call (and its getKey lookup) runs.
type ProviderConfigKeyPropagator struct{}

// NewProviderConfigKeyPropagator returns a ContextPropagator that carries the
// gateway provider config key through Temporal.
func NewProviderConfigKeyPropagator() workflow.ContextPropagator {
	return &ProviderConfigKeyPropagator{}
}

// Inject reads the key from the outbound context.Context (client side) and
// writes it into the Temporal header.
func (p *ProviderConfigKeyPropagator) Inject(ctx context.Context, writer workflow.HeaderWriter) error {
	return p.write(gateway.ProviderConfigKeyFromContext(ctx), writer)
}

// Extract reads the key from the Temporal header into the activity-side
// context.Context, storing it where the LLM client's getKey reads it.
func (p *ProviderConfigKeyPropagator) Extract(ctx context.Context, reader workflow.HeaderReader) (context.Context, error) {
	key, ok, err := p.read(reader)
	if err != nil || !ok {
		return ctx, err
	}
	return gateway.WithProviderConfigKey(ctx, key), nil
}

// InjectFromWorkflow reads the key from the workflow.Context and writes it into
// the Temporal header when scheduling an activity or child workflow.
func (p *ProviderConfigKeyPropagator) InjectFromWorkflow(ctx workflow.Context, writer workflow.HeaderWriter) error {
	key, _ := ctx.Value(workflowProviderConfigKey{}).(string)
	return p.write(key, writer)
}

// ExtractToWorkflow reads the key from the Temporal header into the
// workflow.Context so it can be re-injected when the workflow schedules work.
func (p *ProviderConfigKeyPropagator) ExtractToWorkflow(ctx workflow.Context, reader workflow.HeaderReader) (workflow.Context, error) {
	key, ok, err := p.read(reader)
	if err != nil || !ok {
		return ctx, err
	}
	return workflow.WithValue(ctx, workflowProviderConfigKey{}, key), nil
}

func (p *ProviderConfigKeyPropagator) write(key string, writer workflow.HeaderWriter) error {
	if key == "" {
		return nil
	}
	payload, err := converter.GetDefaultDataConverter().ToPayload(key)
	if err != nil {
		return err
	}
	writer.Set(providerConfigKeyHeader, payload)
	return nil
}

func (p *ProviderConfigKeyPropagator) read(reader workflow.HeaderReader) (string, bool, error) {
	payload, ok := reader.Get(providerConfigKeyHeader)
	if !ok {
		return "", false, nil
	}
	var key string
	if err := converter.GetDefaultDataConverter().FromPayload(payload, &key); err != nil {
		return "", false, err
	}
	return key, true, nil
}
