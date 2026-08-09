package sdk

import (
	"context"
	"testing"

	"github.com/hastekit/hastekit-sdk-go/pkg/agents"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/runtime/restate_runtime"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/runtime/temporal_runtime"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/streambroker"
)

// Registering an agent on a durable runtime must not change the broker it
// was given. Stopping a run is the same everywhere now — record the flag —
// and the tool wrapper does the interrupting wherever the tool runs, so a
// per-runtime broker wrapper would be machinery with nothing to do.
func TestWithRuntime_KeepsTheBrokerItWasGiven(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()
	temporalRT := &TemporalRuntime{
		TemporalRuntime: temporal_runtime.NewTemporalRuntime(nil, broker),
		broker:          broker,
	}
	restateRT := &RestateRuntime{
		RestateRuntime: restate_runtime.NewRestateRuntime("http://localhost:8080", broker),
		broker:         broker,
	}

	for name, runtime := range map[string]agents.Runtime{
		"temporal": temporalRT,
		"restate":  restateRT,
		"plain":    plainRuntime{},
	} {
		opts := &agents.AgentOptions{Name: name + "-agent"}
		WithRuntime(runtime, broker)(opts)

		if opts.StreamBroker != agents.StreamBroker(broker) {
			t.Fatalf("%s: agent got %T; want the broker it was given", name, opts.StreamBroker)
		}
		if opts.Runtime != runtime {
			t.Fatalf("%s: runtime was not set on the agent options", name)
		}
	}
}

type plainRuntime struct{}

func (plainRuntime) Run(ctx context.Context, agent *agents.Agent, in *agents.AgentInput) (*agents.AgentOutput, error) {
	return nil, nil
}
