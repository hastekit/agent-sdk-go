package sdk

import (
	"context"
	"errors"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/local_runtime"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/restate_runtime"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/temporal_runtime"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
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
		"local":    local_runtime.NewLocalRuntime(broker),
		"temporal": temporalRT,
		"restate":  restateRT,
		"plain":    plainRuntime{broker: broker},
	} {
		opts := &agents.AgentOptions{Name: name + "-agent"}
		WithRuntime(runtime)(opts)

		if opts.StreamBroker != agents.StreamBroker(broker) {
			t.Fatalf("%s: agent got %T; want the broker it was given", name, opts.StreamBroker)
		}
		if runtime.StreamBroker() != broker {
			t.Fatalf("%s: runtime returned a different broker", name)
		}
		if opts.Runtime != runtime {
			t.Fatalf("%s: runtime was not set on the agent options", name)
		}
	}
}

type plainRuntime struct{ broker agents.StreamBroker }

func (r plainRuntime) StreamBroker() agents.StreamBroker { return r.broker }

func (plainRuntime) Run(ctx context.Context, agent *agents.Agent, in *agents.AgentInput) (*agents.AgentOutput, error) {
	return nil, nil
}

func TestNewAgentRegistersResolvedRuntimeConfiguration(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()
	temporalRT := &TemporalRuntime{TemporalRuntime: temporal_runtime.NewTemporalRuntime(nil, broker), broker: broker}
	restateRT := &RestateRuntime{RestateRuntime: restate_runtime.NewRestateRuntime("http://localhost:8080", broker), broker: broker}
	model := NewLLMClient(testConfigs()).Model("OpenAI/test")
	for _, tc := range []struct {
		name      string
		runtime   agents.Runtime
		lifecycle *runtimeLifecycle
	}{
		{"temporal", temporalRT, &temporalRT.lifecycle},
		{"restate", restateRT, &restateRT.lifecycle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &AgentConfig{Name: "original", LLM: model}
			agent, err := NewAgent(cfg, WithRuntime(tc.runtime), func(opts *agents.AgentOptions) { opts.Name = "resolved" })
			if err != nil {
				t.Fatal(err)
			}
			registered := tc.lifecycle.configs["resolved"]
			if agent.Name != "resolved" || registered == nil || len(tc.lifecycle.configs) != 1 {
				t.Fatalf("resolved configuration was not registered: %#v", tc.lifecycle.configs)
			}
			if registered.Runtime != tc.runtime || registered.StreamBroker != broker || registered.LLM != model {
				t.Fatal("registration did not retain the resolved dependencies")
			}
			if cfg.Name != "original" {
				t.Fatal("constructor mutated caller configuration")
			}
			_, err = NewAgent(&AgentConfig{Name: "invalid"}, WithRuntime(tc.runtime))
			if err == nil || len(tc.lifecycle.configs) != 1 {
				t.Fatal("invalid configuration must not be registered")
			}
			_, err = NewAgent(&AgentConfig{Name: "resolved", LLM: model}, WithRuntime(tc.runtime))
			if !errors.Is(err, ErrAgentAlreadyRegistered) {
				t.Fatalf("expected duplicate error, got %v", err)
			}
		})
	}
	if _, err := NewAgent(&AgentConfig{Name: "plain", LLM: model}, WithRuntime(plainRuntime{broker: broker})); err != nil {
		t.Fatalf("custom runtime: %v", err)
	}
}

func TestNewAgentWithLocalRuntimeBroker(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()
	runtime := local_runtime.NewLocalRuntime(broker)
	cfg := &AgentConfig{Name: "local", LLM: NewLLMClient(testConfigs()).Model("OpenAI/test")}
	agent, err := NewAgent(cfg, WithRuntime(runtime))
	if err != nil {
		t.Fatal(err)
	}
	if agent.StreamBroker() != broker {
		t.Fatal("agent must use runtime broker")
	}
	agent, err = NewAgent(cfg, WithRuntime(runtime), WithRuntime(nil))
	if err != nil {
		t.Fatal(err)
	}
	if agent.StreamBroker() == nil || agent.StreamBroker() == broker {
		t.Fatal("nil runtime should restore the default local broker")
	}
}

func (plainRuntime) RegisterAgent(*agents.AgentOptions) error { return nil }

// A runtime implemented outside the SDK's constructor path receives the same
// registration callback and can reject a configuration.
type registeringRuntime struct {
	plainRuntime
	options *agents.AgentOptions
	err     error
	calls   int
}

func (r *registeringRuntime) RegisterAgent(options *agents.AgentOptions) error {
	r.calls++
	r.options = options
	return r.err
}
func TestNewAgentCustomRuntimeRegistration(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()
	rt := &registeringRuntime{plainRuntime: plainRuntime{broker: broker}}
	cfg := &AgentConfig{Name: "custom", LLM: NewLLMClient(testConfigs()).Model("OpenAI/test")}
	agent, err := NewAgent(cfg, WithRuntime(rt), func(opts *agents.AgentOptions) { opts.Name = "resolved" })
	if err != nil {
		t.Fatal(err)
	}
	if rt.calls != 1 || rt.options.Name != agent.Name || rt.options.StreamBroker != broker || rt.options.Runtime != rt {
		t.Fatal("runtime did not receive resolved configuration exactly once")
	}
	rt.err = errors.New("registration failed")
	agent, err = NewAgent(cfg, WithRuntime(rt))
	if agent != nil || !errors.Is(err, rt.err) {
		t.Fatalf("registration failure: agent=%v err=%v", agent, err)
	}
	_, err = NewAgent(&AgentConfig{Name: "invalid"}, WithRuntime(rt))
	if err == nil || rt.calls != 2 {
		t.Fatal("invalid configuration reached runtime registration")
	}
}
