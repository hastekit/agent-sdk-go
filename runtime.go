package sdk

import (
	"context"
	"fmt"
	"log"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/restate_runtime"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/temporal_runtime"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
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
	temporalEndpoint string
	redisEndpoint    string
	broker           agents.StreamBroker
}

// NewTemporalRuntime creates a new Temporal runtime
func NewTemporalRuntime(temporalEndpoint string, redisEndpoint string) (*TemporalRuntime, error) {
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

	// Create a redis broker
	broker, err := streambroker.NewRedisStreamBroker(streambroker.RedisStreamBrokerOptions{
		Addr: redisEndpoint,
	})
	if err != nil {
		return nil, fmt.Errorf("error creating redis stream broker: %w", err)
	}

	return &TemporalRuntime{
		TemporalRuntime:  temporal_runtime.NewTemporalRuntime(cli, broker),
		temporalEndpoint: temporalEndpoint,
		redisEndpoint:    redisEndpoint,
		broker:           broker,
	}, nil
}

func (r *TemporalRuntime) Start() {
	tracingInterceptor, err := opentelemetry.NewTracingInterceptor(opentelemetry.TracerOptions{})
	if err != nil {
		log.Fatalln("Unable to create interceptor", err)
	}

	cli, err := client.Dial(client.Options{
		HostPort:     r.temporalEndpoint,
		Interceptors: []interceptor.ClientInterceptor{tracingInterceptor},
		ContextPropagators: []workflow.ContextPropagator{
			temporal_runtime.NewProviderConfigKeyPropagator(),
		},
	})
	if err != nil {
		panic("unable to create temporal client")
	}

	go func() {
		w := worker.New(cli, "AgentWorkflowTaskQueue", worker.Options{})

		// Register workflows and activities based on the agents available in the SDK
		for agentName, agentOptions := range temporalAgentConfigs {
			temporalAgentProxy := temporal_runtime.NewTemporalAgent(temporalAgentConfigs, agentOptions, r.broker)
			for name, fn := range temporalAgentProxy.GetActivities() {
				w.RegisterActivityWithOptions(fn, activity.RegisterOptions{Name: name})
			}
			w.RegisterWorkflowWithOptions(temporalAgentProxy.Execute, workflow.RegisterOptions{
				Name: agentName + "_AgentWorkflow",
			})
		}

		err = w.Run(worker.InterruptCh())
		if err != nil {
			log.Fatal(err)
		}
	}()
}

type RestateRuntime struct {
	*restate_runtime.RestateRuntime
	restateEndpoint string
	redisEndpoint   string
	broker          agents.StreamBroker
}

func NewRestateRuntime(restateEndpoint string, redisEndpoint string) (*RestateRuntime, error) {
	// Create a redis broker
	broker, err := streambroker.NewRedisStreamBroker(streambroker.RedisStreamBrokerOptions{
		Addr: redisEndpoint,
	})
	if err != nil {
		return nil, fmt.Errorf("error creating redis stream broker: %w", err)
	}

	return &RestateRuntime{
		RestateRuntime:  restate_runtime.NewRestateRuntime(restateEndpoint, broker),
		broker:          broker,
		restateEndpoint: "localhost:8082",
		redisEndpoint:   redisEndpoint,
	}, nil
}

func (r *RestateRuntime) Start() {
	wf := restate_runtime.NewRestateWorkflow(restateAgentConfigs, r.broker)

	go func() {
		if err := server.NewRestate().
			Bind(restate.Reflect(wf)).
			Start(context.Background(), "localhost:8082"); err != nil {
			log.Fatal(err)
		}
	}()
}
