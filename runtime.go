package sdk

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/restate_runtime"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/temporal_runtime"
	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/server"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/contrib/opentelemetry"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

type TemporalRuntime struct {
	*temporal_runtime.TemporalRuntime
	lifecycle runtimeLifecycle
	client    client.Client
	closeOnce sync.Once
	broker    agents.StreamBroker
}

// NewTemporalRuntime requires an explicit broker. Use a shared broker such as
// Redis when the caller and workers run in different processes.
func NewTemporalRuntime(temporalEndpoint string, broker agents.StreamBroker) (*TemporalRuntime, error) {
	if temporalEndpoint == "" {
		return nil, fmt.Errorf("temporal endpoint is required")
	}
	if broker == nil {
		return nil, fmt.Errorf("an explicit stream broker is required for the runtime")
	}
	otelInterceptor, err := opentelemetry.NewTracingInterceptor(
		opentelemetry.TracerOptions{},
	)
	if err != nil {
		return nil, err
	}

	// Create a temporal client
	cli, err := client.Dial(client.Options{
		HostPort: temporalEndpoint,
		Interceptors: []interceptor.ClientInterceptor{
			otelInterceptor,
		},
		ContextPropagators: []workflow.ContextPropagator{
			temporal_runtime.NewProviderConfigKeyPropagator(),
		},
	})
	if err != nil {
		return nil, err
	}

	return &TemporalRuntime{
		TemporalRuntime: temporal_runtime.NewTemporalRuntime(cli, broker),
		client:          cli,
		broker:          broker,
	}, nil
}

// RegisterAgent registers an agent configuration before Serve starts.
func (r *TemporalRuntime) RegisterAgent(options *agents.AgentOptions) error {
	return r.lifecycle.register(options)
}

// Serve runs the worker until ctx ends or startup fails. The caller owns the
// runtime and must Close it after Serve and outstanding executions finish.
func (r *TemporalRuntime) Serve(ctx context.Context) error {
	ctx, configs, finish, err := r.lifecycle.begin(ctx)
	if err != nil {
		return err
	}
	defer finish()
	if r.client == nil {
		return fmt.Errorf("no temporal client available")
	}
	w := worker.New(r.client, "AgentWorkflowTaskQueue", worker.Options{})
	for _, options := range configs {
		proxy := temporal_runtime.NewTemporalAgent(configs, options, r.broker)
		for name, fn := range proxy.GetActivities() {
			w.RegisterActivityWithOptions(fn, activity.RegisterOptions{Name: name})
		}
		for name, fn := range proxy.GetWorkflows() {
			w.RegisterWorkflowWithOptions(fn, workflow.RegisterOptions{Name: name})
		}
	}
	if err := w.Start(); err != nil {
		return err
	}
	defer w.Stop()
	<-ctx.Done()
	return nil
}

// Close stops the worker and closes the client opened by NewTemporalRuntime.
// Brokers and histories supplied by the caller are not closed.
func (r *TemporalRuntime) Close() error {
	r.lifecycle.close()
	r.closeOnce.Do(func() {
		if r.client != nil {
			r.client.Close()
		}
	})
	return nil
}

type RestateRuntime struct {
	*restate_runtime.RestateRuntime
	lifecycle runtimeLifecycle
	broker    agents.StreamBroker
}

// NewRestateRuntime requires an explicit broker shared by callers and service
// processes. An in-memory broker is only suitable for a single-process setup.
func NewRestateRuntime(restateEndpoint string, broker agents.StreamBroker) (*RestateRuntime, error) {
	if restateEndpoint == "" {
		return nil, fmt.Errorf("restate endpoint is required")
	}
	if broker == nil {
		return nil, fmt.Errorf("an explicit stream broker is required for the runtime")
	}
	return &RestateRuntime{
		RestateRuntime: restate_runtime.NewRestateRuntime(restateEndpoint, broker),
		broker:         broker,
	}, nil
}

// RegisterAgent registers an agent configuration before Serve starts.
func (r *RestateRuntime) RegisterAgent(options *agents.AgentOptions) error {
	return r.lifecycle.register(options)
}

// Serve blocks serving Restate invocations at address until ctx ends. The
// address is the local listen address, separate from the ingress endpoint.
func (r *RestateRuntime) Serve(ctx context.Context, address string) error {
	ctx, configs, finish, err := r.lifecycle.begin(ctx)
	if err != nil {
		return err
	}
	defer finish()
	if address == "" {
		return fmt.Errorf("restate listen address is required")
	}
	wf := restate_runtime.NewRestateWorkflow(configs, r.broker)
	background := restate_runtime.NewBackgroundTaskService(configs, r.broker)
	handler, err := server.NewRestate().Bind(restate.Reflect(wf)).Bind(restate.Reflect(background)).Handler()
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: handler, Protocols: &protocols, ReadHeaderTimeout: 5 * time.Second}
	shutdownDone := make(chan error, 1)
	stop := context.AfterFunc(ctx, func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := srv.Shutdown(shutdownCtx)
		if err != nil {
			_ = srv.Close()
		}
		shutdownDone <- err
	})
	err = srv.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	if !stop() {
		err = errors.Join(err, <-shutdownDone)
	}
	return err
}

func (r *RestateRuntime) Close() error { r.lifecycle.close(); return nil }

// runtimeLifecycle owns configuration and worker lifetime for one instance.
type runtimeLifecycle struct {
	mu      sync.Mutex
	configs map[string]*agents.AgentOptions
	cancel  context.CancelFunc
	done    chan struct{}
	closed  bool
}

// register stores the validated configuration resolved by NewAgent. Registration
// and worker startup share a lock so Serve sees a complete configuration snapshot.
func (r *runtimeLifecycle) register(options *agents.AgentOptions) error {
	if options == nil {
		return fmt.Errorf("agent options are nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.cancel != nil {
		return fmt.Errorf("runtime is serving or closed")
	}
	if _, exists := r.configs[options.Name]; exists {
		return fmt.Errorf("%w: %s", ErrAgentAlreadyRegistered, options.Name)
	}
	if r.configs == nil {
		r.configs = make(map[string]*agents.AgentOptions)
	}
	r.configs[options.Name] = options
	return nil
}

func (r *runtimeLifecycle) begin(ctx context.Context) (context.Context, map[string]*agents.AgentOptions, func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	if r.closed || r.cancel != nil {
		return nil, nil, nil, fmt.Errorf("runtime is serving or closed")
	}
	if len(r.configs) == 0 {
		return nil, nil, nil, fmt.Errorf("runtime has no registered agents")
	}
	ctx, r.cancel = context.WithCancel(ctx)
	r.done = make(chan struct{})
	configs := make(map[string]*agents.AgentOptions, len(r.configs))
	for name, cfg := range r.configs {
		configs[name] = cfg
	}
	finish := func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.cancel()
		r.cancel = nil
		close(r.done)
	}
	return ctx, configs, finish, nil
}

func (r *runtimeLifecycle) close() {
	r.mu.Lock()
	r.closed = true
	done := r.done
	if r.cancel != nil {
		r.cancel()
	}
	r.mu.Unlock()
	if done != nil {
		<-done
	}
}

// StreamBroker returns the broker configured for this runtime.
func (r *TemporalRuntime) StreamBroker() agents.StreamBroker { return r.broker }

// StreamBroker returns the broker configured for this runtime.
func (r *RestateRuntime) StreamBroker() agents.StreamBroker { return r.broker }
